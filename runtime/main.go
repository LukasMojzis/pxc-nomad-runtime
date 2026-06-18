package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func requireEnv(name string) (string, error) {
	if value := os.Getenv(name); value != "" {
		return value, nil
	}
	return "", fmt.Errorf("%s is required", name)
}

func resolveSRV(name string) ([]string, error) {
	_, records, err := net.LookupSRV("", "", name)
	if err != nil {
		return nil, fmt.Errorf("SRV lookup for %s failed: %w", name, err)
	}
	peers := sortedPeers(records)
	if len(peers) == 0 {
		return nil, fmt.Errorf("SRV lookup for %s returned no usable records", name)
	}
	return peers, nil
}

func sortedPeers(records []*net.SRV) []string {
	seen := map[string]bool{}
	peers := make([]string, 0, len(records))
	sort.Slice(records, func(i, j int) bool {
		if records[i].Priority != records[j].Priority {
			return records[i].Priority < records[j].Priority
		}
		if records[i].Weight != records[j].Weight {
			return records[i].Weight > records[j].Weight
		}
		return records[i].Target < records[j].Target
	})
	for _, record := range records {
		target := strings.ToLower(strings.TrimSuffix(record.Target, "."))
		item := fmt.Sprintf("%s:%d", target, record.Port)
		if !seen[item] {
			peers = append(peers, item)
			seen[item] = true
		}
	}
	return peers
}

func shouldBootstrap() bool {
	if os.Getenv("PXC_BOOTSTRAP") == "1" {
		return true
	}
	key := env("PXC_BOOTSTRAP_KEY", "pxc/control/bootstrap")
	base := strings.TrimRight(env("CONSUL_HTTP_ADDR", "http://127.0.0.1:8500"), "/")
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/v1/kv/" + key + "?raw")
	if err != nil || resp.StatusCode == http.StatusNotFound {
		return false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	value := strings.TrimSpace(string(body))
	for _, candidate := range []string{os.Getenv("NOMAD_ALLOC_ID"), os.Getenv("DC"), os.Getenv("NOMAD_IP_galera")} {
		if value != "" && value == candidate {
			return true
		}
	}
	return value == "1"
}

func runPXC() error {
	if shouldBootstrap() {
		os.Unsetenv("CLUSTER_JOIN")
		fmt.Fprintln(os.Stderr, "pxc-runtime: bootstrap mode, CLUSTER_JOIN unset")
	} else {
		joinDNS, err := requireEnv("JOIN_DNS")
		if err != nil {
			return err
		}
		peers, err := resolveSRV(joinDNS)
		if err != nil {
			return err
		}
		join := strings.Join(peers, ",")
		os.Setenv("CLUSTER_JOIN", join)
		fmt.Fprintf(os.Stderr, "pxc-runtime: CLUSTER_JOIN=%s\n", join)
	}
	args := []string{
		"/entrypoint.sh",
		"mysqld",
		"--pxc-encrypt-cluster-traffic=OFF",
		"--wsrep-node-address=" + os.Getenv("NOMAD_IP_galera"),
		"--wsrep-node-incoming-address=" + os.Getenv("NOMAD_IP_mysql") + ":3306",
	}
	return syscallExec(args)
}

func runGarbd() error {
	joinDNS, err := requireEnv("JOIN_DNS")
	if err != nil {
		return err
	}
	group, err := requireEnv("GARBD_GROUP")
	if err != nil {
		return err
	}
	peers, err := resolveSRV(joinDNS)
	if err != nil {
		return err
	}
	address := "gcomm://" + strings.Join(peers, ",")
	args := []string{
		"garbd",
		"--address", address,
		"--group", group,
		"--name", env("GARBD_NAME", "garbd"),
		"--log", env("GARBD_LOG", "/dev/stdout"),
		"--options", env("GARBD_OPTIONS", "gcs.fc_limit=9999999;gcs.fc_factor=1.0;gcs.fc_single_primary=yes;gcs.stateless=yes;"),
	}
	fmt.Fprintf(os.Stderr, "pxc-runtime: garbd address=%s\n", address)
	return syscallExec(args)
}

func syscallExec(args []string) error {
	binary, err := exec.LookPath(args[0])
	if err != nil {
		return err
	}
	return syscall.Exec(binary, args, os.Environ())
}

func mysqlStatus(names []string) (map[string]string, error) {
	dsn := fmt.Sprintf("root:%s@tcp(127.0.0.1:3306)/", os.Getenv("MYSQL_ROOT_PASSWORD"))
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, "'"+name+"'")
	}
	rows, err := db.QueryContext(ctx, "SHOW GLOBAL STATUS WHERE Variable_name IN ("+strings.Join(quoted, ",")+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		values[name] = value
	}
	return values, rows.Err()
}

func checkPrimary() int {
	values, err := mysqlStatus([]string{"wsrep_cluster_status", "wsrep_connected"})
	if err != nil {
		fmt.Println(err)
		return 1
	}
	fmt.Printf("cluster_status=%s connected=%s\n", values["wsrep_cluster_status"], values["wsrep_connected"])
	if values["wsrep_cluster_status"] == "Primary" && values["wsrep_connected"] == "ON" {
		return 0
	}
	return 1
}

func checkReady() int {
	values, err := mysqlStatus([]string{"wsrep_cluster_status", "wsrep_local_state_comment", "wsrep_ready"})
	if err != nil {
		fmt.Println(err)
		return 1
	}
	fmt.Printf("cluster_status=%s local_state=%s ready=%s\n", values["wsrep_cluster_status"], values["wsrep_local_state_comment"], values["wsrep_ready"])
	if values["wsrep_cluster_status"] == "Primary" && values["wsrep_local_state_comment"] == "Synced" && values["wsrep_ready"] == "ON" {
		return 0
	}
	return 1
}

func recoverPosition() int {
	cmd := exec.Command("mysqld", "--wsrep_recover", "--log-error-verbosity=3", "--log_error=/dev/stderr", "--wsrep-provider=/usr/lib64/galera4/libgalera_smm.so", "--pxc-encrypt-cluster-traffic=OFF")
	output, _ := cmd.CombinedOutput()
	lines := strings.Split(string(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if idx := strings.Index(lines[i], "Recovered position:"); idx >= 0 {
			fmt.Println(strings.TrimSpace(lines[i][idx+len("Recovered position:"):]))
			return 0
		}
	}
	fmt.Println("00000000-0000-0000-0000-000000000000:-1")
	return 0
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pxc-runtime <run-pxc|run-garbd|check-primary|check-ready|recover-position|resolve-srv>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run-pxc":
		if err := runPXC(); err != nil {
			fmt.Fprintln(os.Stderr, "pxc-runtime:", err)
			os.Exit(1)
		}
	case "run-garbd":
		if err := runGarbd(); err != nil {
			fmt.Fprintln(os.Stderr, "pxc-runtime:", err)
			os.Exit(1)
		}
	case "check-primary":
		os.Exit(checkPrimary())
	case "check-ready":
		os.Exit(checkReady())
	case "recover-position":
		os.Exit(recoverPosition())
	case "resolve-srv":
		if len(os.Args) != 3 {
			os.Exit(2)
		}
		peers, err := resolveSRV(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, "pxc-runtime:", err)
			os.Exit(1)
		}
		fmt.Println(strings.Join(peers, ","))
	default:
		os.Exit(2)
	}
}

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
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

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func statusInt(values map[string]string, name string) int {
	parsed, err := strconv.Atoi(values[name])
	if err != nil {
		return 0
	}
	return parsed
}

func resolveSRV(name string) ([]string, error) {
	resolver := consulDNSResolver()
	_, records, err := resolver.LookupSRV(context.Background(), "", "", name)
	if err != nil {
		return nil, fmt.Errorf("SRV lookup for %s failed: %w", name, err)
	}
	peers := sortedPeers(records)
	if len(peers) == 0 {
		return nil, fmt.Errorf("SRV lookup for %s returned no usable records", name)
	}
	return peers, nil
}

func consulDNSResolver() *net.Resolver {
	address := env("CONSUL_DNS_ADDR", "127.0.0.1:8600")
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialer := net.Dialer{Timeout: 2 * time.Second}
			return dialer.DialContext(ctx, network, address)
		},
	}
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

func consulBase() string {
	return strings.TrimRight(env("CONSUL_HTTP_ADDR", "http://127.0.0.1:8500"), "/")
}

func shouldBootstrap() bool {
	if os.Getenv("PXC_BOOTSTRAP") == "1" {
		return true
	}
	key := env("PXC_BOOTSTRAP_KEY", "pxc/control/bootstrap")
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(consulBase() + "/v1/kv/" + key + "?raw")
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

func pxcArgs() []string {
	return []string{
		"/entrypoint.sh",
		"mysqld",
		"--pxc-encrypt-cluster-traffic=OFF",
		"--wsrep-node-address=" + os.Getenv("NOMAD_IP_galera"),
		"--wsrep-node-incoming-address=" + os.Getenv("NOMAD_IP_mysql") + ":3306",
	}
}

func resolveJoin() (string, error) {
	joinDNS, err := requireEnv("JOIN_DNS")
	if err != nil {
		return "", err
	}
	peers, err := resolveSRV(joinDNS)
	if err != nil {
		return "", err
	}
	return strings.Join(peers, ","), nil
}

func normalStart() error {
	join, err := resolveJoin()
	if err != nil {
		return err
	}
	os.Setenv("CLUSTER_JOIN", join)
	fmt.Fprintf(os.Stderr, "pxc-runtime: normal start CLUSTER_JOIN=%s\n", join)
	args := pxcArgs()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func joinStart() error {
	join, err := resolveJoin()
	if err != nil {
		return err
	}
	os.Setenv("CLUSTER_JOIN", join)
	fmt.Fprintf(os.Stderr, "pxc-runtime: join start CLUSTER_JOIN=%s\n", join)
	return syscallExec(pxcArgs())
}

func bootstrapStart() error {
	os.Unsetenv("CLUSTER_JOIN")
	if err := setSafeToBootstrap(1); err != nil {
		fmt.Fprintf(os.Stderr, "pxc-runtime: failed to mark safe_to_bootstrap=1: %v\n", err)
	}
	fmt.Fprintln(os.Stderr, "pxc-runtime: bootstrap mode, CLUSTER_JOIN unset")
	return syscallExec(pxcArgs())
}

func datadir() string {
	return env("DATADIR", "/var/lib/mysql")
}

func grastatePath() string {
	return strings.TrimRight(datadir(), "/") + "/grastate.dat"
}

func safeToBootstrapIs1() bool {
	body, err := os.ReadFile(grastatePath())
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "safe_to_bootstrap:" && fields[1] == "1" {
			return true
		}
	}
	return false
}

func setSafeToBootstrap(value int) error {
	path := grastatePath()
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(body), "\n")
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(line, "safe_to_bootstrap:") {
			lines[i] = fmt.Sprintf("safe_to_bootstrap: %d", value)
			replaced = true
		}
	}
	if !replaced {
		lines = append(lines, fmt.Sprintf("safe_to_bootstrap: %d", value))
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

func healthyPrimaryExists() bool {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(consulBase() + "/v1/health/service/pxc-primary?passing=1")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	value := strings.TrimSpace(string(body))
	return value != "" && value != "[]"
}

func bootstrapBase() string {
	return "pxc/bootstrap/" + env("CLUSTER_NAME", "pxc")
}

func candidateKeys() ([]string, error) {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(consulBase() + "/v1/kv/" + bootstrapBase() + "/candidates/?keys")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var keys []string
	if err := json.Unmarshal(body, &keys); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

func candidateCount() int {
	keys, err := candidateKeys()
	if err != nil {
		return 0
	}
	return len(keys)
}

func recoverPositionValue() string {
	logFile, err := os.CreateTemp(datadir(), "wsrep_recover.*.log")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pxc-runtime: failed to create recovery log: %v\n", err)
		return "00000000-0000-0000-0000-000000000000:-1"
	}
	logPath := logFile.Name()
	logFile.Close()
	defer os.Remove(logPath)

	cmd := exec.Command("mysqld", "--wsrep_recover", "--log-error-verbosity=3", "--log_error="+logPath, "--wsrep-provider=/usr/lib64/galera4/libgalera_smm.so", "--pxc-encrypt-cluster-traffic=OFF")
	output, _ := cmd.CombinedOutput()
	logBody, _ := os.ReadFile(logPath)
	lines := strings.Split(string(logBody), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if idx := strings.Index(lines[i], "Recovered position:"); idx >= 0 {
			return strings.TrimSpace(lines[i][idx+len("Recovered position:"):])
		}
	}
	fmt.Fprint(os.Stderr, string(output))
	fmt.Fprint(os.Stderr, string(logBody))
	return "00000000-0000-0000-0000-000000000000:-1"
}

func publishCandidate(position string) error {
	parts := strings.Split(position, ":")
	if len(parts) != 2 {
		return fmt.Errorf("invalid recovered position %q", position)
	}
	dc, err := requireEnv("DC")
	if err != nil {
		return err
	}
	selfIP, err := requireEnv("NOMAD_IP_galera")
	if err != nil {
		return err
	}
	value := fmt.Sprintf("%s:%s:%s:%s:%d", parts[0], parts[1], dc, selfIP, time.Now().Unix())
	req, err := http.NewRequest(http.MethodPut, consulBase()+"/v1/kv/"+bootstrapBase()+"/candidates/"+dc, strings.NewReader(value))
	if err != nil {
		return err
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("candidate publish returned HTTP %d", resp.StatusCode)
	}
	fmt.Fprintf(os.Stderr, "pxc-runtime: published recovery candidate dc=%s ip=%s position=%s\n", dc, selfIP, position)
	return nil
}

func waitForRecoveryView() {
	deadline := time.Now().Add(time.Duration(envInt("CANDIDATE_MAX_WAIT_SECONDS", 45)) * time.Second)
	stableFor := time.Duration(envInt("CANDIDATE_STABLE_SECONDS", 5)) * time.Second
	lastCount := -1
	var stableSince time.Time
	for {
		if healthyPrimaryExists() {
			return
		}
		count := candidateCount()
		fmt.Fprintf(os.Stderr, "pxc-runtime: recovery candidates=%d\n", count)
		now := time.Now()
		if count > 0 && count == lastCount {
			if stableSince.IsZero() {
				stableSince = now
			}
			if now.Sub(stableSince) >= stableFor {
				return
			}
		} else {
			lastCount = count
			stableSince = time.Time{}
		}
		if now.After(deadline) {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

type recoveryCandidate struct {
	uuid string
	seq  int64
	dc   string
	ip   string
}

func readCandidate(key string) (recoveryCandidate, bool) {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(consulBase() + "/v1/kv/" + key + "?raw")
	if err != nil {
		return recoveryCandidate{}, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return recoveryCandidate{}, false
	}
	parts := strings.Split(strings.TrimSpace(string(body)), ":")
	if len(parts) != 5 {
		return recoveryCandidate{}, false
	}
	seq, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return recoveryCandidate{}, false
	}
	ts, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return recoveryCandidate{}, false
	}
	if time.Now().Unix()-ts > int64(envInt("CANDIDATE_MAX_AGE_SECONDS", 180)) {
		return recoveryCandidate{}, false
	}
	return recoveryCandidate{uuid: parts[0], seq: seq, dc: parts[2], ip: parts[3]}, true
}

func electWinner() (recoveryCandidate, error) {
	keys, err := candidateKeys()
	if err != nil {
		return recoveryCandidate{}, err
	}
	var winner recoveryCandidate
	found := false
	for _, key := range keys {
		candidate, ok := readCandidate(key)
		if !ok {
			continue
		}
		if !found || candidate.seq > winner.seq || (candidate.seq == winner.seq && candidate.dc > winner.dc) {
			winner = candidate
			found = true
		}
	}
	if !found {
		return recoveryCandidate{}, fmt.Errorf("no valid recovery candidates found")
	}
	return winner, nil
}

func runPXC() error {
	if shouldBootstrap() {
		return bootstrapStart()
	}
	err := normalStart()
	if err == nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "pxc-runtime: normal start failed: %v\n", err)
	if healthyPrimaryExists() {
		return joinStart()
	}
	position := recoverPositionValue()
	if err := publishCandidate(position); err != nil {
		return err
	}
	waitForRecoveryView()
	if healthyPrimaryExists() {
		return joinStart()
	}
	winner, err := electWinner()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "pxc-runtime: elected dc=%s ip=%s uuid=%s seqno=%d\n", winner.dc, winner.ip, winner.uuid, winner.seq)
	if winner.dc == os.Getenv("DC") {
		return bootstrapStart()
	}
	for !healthyPrimaryExists() {
		fmt.Fprintf(os.Stderr, "pxc-runtime: waiting for elected bootstrapper dc=%s ip=%s\n", winner.dc, winner.ip)
		time.Sleep(2 * time.Second)
	}
	return joinStart()
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
	values, err := mysqlStatus([]string{"wsrep_cluster_status", "wsrep_connected", "wsrep_cluster_size"})
	if err != nil {
		fmt.Println(err)
		return 1
	}
	size := statusInt(values, "wsrep_cluster_size")
	minSize := envInt("PXC_MIN_CLUSTER_SIZE", 1)
	fmt.Printf("cluster_status=%s connected=%s cluster_size=%d min_cluster_size=%d\n", values["wsrep_cluster_status"], values["wsrep_connected"], size, minSize)
	if values["wsrep_cluster_status"] == "Primary" && values["wsrep_connected"] == "ON" && size >= minSize {
		return 0
	}
	return 1
}

func checkReady() int {
	values, err := mysqlStatus([]string{"wsrep_cluster_status", "wsrep_local_state_comment", "wsrep_ready", "wsrep_cluster_size"})
	if err != nil {
		fmt.Println(err)
		return 1
	}
	size := statusInt(values, "wsrep_cluster_size")
	minSize := envInt("PXC_MIN_CLUSTER_SIZE", 1)
	fmt.Printf("cluster_status=%s local_state=%s ready=%s cluster_size=%d min_cluster_size=%d\n", values["wsrep_cluster_status"], values["wsrep_local_state_comment"], values["wsrep_ready"], size, minSize)
	if values["wsrep_cluster_status"] == "Primary" && values["wsrep_local_state_comment"] == "Synced" && values["wsrep_ready"] == "ON" && size >= minSize {
		return 0
	}
	return 1
}

func recoverPosition() int {
	fmt.Println(recoverPositionValue())
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

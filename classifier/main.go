package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultMinRAMMB       = 8192
	defaultMinFreeBytes   = int64(20 * 1024 * 1024 * 1024)
	defaultInterval       = 60 * time.Second
	defaultHostRoot       = "/host"
	defaultHostPXCDataDir = "/root/docker/pxc/data"
)

type Node struct {
	ID         string            `json:"ID"`
	Name       string            `json:"Name"`
	Datacenter string            `json:"Datacenter"`
	Attributes map[string]string `json:"Attributes"`
	Meta       map[string]string `json:"Meta"`
}

type Decision struct {
	NodeName         string `json:"node_name"`
	NodeID           string `json:"node_id"`
	Intent           string `json:"intent"`
	MemberRole       string `json:"member_role"`
	Admission        string `json:"admission"`
	Reason           string `json:"reason"`
	StorageStatus    string `json:"storage_status"`
	RAMMB            int64  `json:"ram_mb"`
	StorageFreeBytes int64  `json:"storage_free_bytes"`
	DBBytes          int64  `json:"db_bytes"`
	DataDirExists    bool   `json:"data_dir_exists"`
	ExistingMember   bool   `json:"existing_member"`
	ConsulServer     bool   `json:"consul_server"`
	DockerHealthy    bool   `json:"docker_healthy"`
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt64(name string, fallback int64) int64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func consulBase() string {
	return strings.TrimRight(env("CONSUL_HTTP_ADDR", "http://127.0.0.1:8500"), "/")
}

func nomadBase() string {
	return strings.TrimRight(env("NOMAD_ADDR", "http://127.0.0.1:4646"), "/")
}

func getRaw(url string) ([]byte, int, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

func node() (Node, error) {
	nodeID := env("NODE_ID", "")
	if nodeID == "" {
		return Node{}, errors.New("NODE_ID is required")
	}
	url := fmt.Sprintf("%s/v1/node/%s?region=%s", nomadBase(), nodeID, env("NOMAD_REGION", "global"))
	body, status, err := getRaw(url)
	if err != nil {
		return Node{}, err
	}
	if status != http.StatusOK {
		return Node{}, fmt.Errorf("nomad node read failed: HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	var result Node
	if err := json.Unmarshal(body, &result); err != nil {
		return Node{}, err
	}
	return result, nil
}

func consulRawKey(key string) (string, bool, error) {
	body, status, err := getRaw(consulBase() + "/v1/kv/" + key + "?raw")
	if err != nil {
		return "", false, err
	}
	if status == http.StatusNotFound {
		return "", false, nil
	}
	if status != http.StatusOK {
		return "", false, fmt.Errorf("consul key %s failed: HTTP %d", key, status)
	}
	return strings.TrimSpace(string(body)), true, nil
}

func putConsulJSON(key string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, consulBase()+"/v1/kv/"+key, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("consul put %s failed: HTTP %d: %s", key, resp.StatusCode, strings.TrimSpace(string(text)))
	}
	return nil
}

func memoryMB(node Node) int64 {
	if raw := node.Attributes["memory.totalbytes"]; raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return value / 1024 / 1024
		}
	}
	body, err := os.ReadFile(hostPathToContainer(env("HOST_ROOT", defaultHostRoot), "/proc/meminfo"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseInt(fields[1], 10, 64)
				return kb / 1024
			}
		}
	}
	return 0
}

func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(item string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func statFree(path string) int64 {
	probe := path
	for {
		if _, err := os.Stat(probe); err == nil {
			var stat syscall.Statfs_t
			if syscall.Statfs(probe, &stat) == nil {
				return int64(stat.Bavail) * int64(stat.Bsize)
			}
			return 0
		}
		next := filepath.Dir(probe)
		if next == probe {
			return 0
		}
		probe = next
	}
}

func existingMember(path string) bool {
	for _, name := range []string{"grastate.dat", "galera.cache", "gvwstate.dat"} {
		if _, err := os.Stat(filepath.Join(path, name)); err == nil {
			return true
		}
	}
	return false
}

func hostPathToContainer(root, hostPath string) string {
	cleanRoot := filepath.Clean(root)
	cleanHostPath := filepath.Clean(hostPath)
	if cleanHostPath == "." || cleanHostPath == string(filepath.Separator) {
		return cleanRoot
	}
	return filepath.Join(cleanRoot, strings.TrimPrefix(cleanHostPath, string(filepath.Separator)))
}

func resolveHostPath(root, hostPath string) string {
	current := filepath.Clean(hostPath)
	if !filepath.IsAbs(current) {
		current = string(filepath.Separator) + current
	}
	for depth := 0; depth < 32; depth++ {
		parts := strings.Split(strings.TrimPrefix(current, string(filepath.Separator)), string(filepath.Separator))
		prefix := string(filepath.Separator)
		for i, part := range parts {
			if part == "" {
				continue
			}
			next := filepath.Join(prefix, part)
			containerNext := hostPathToContainer(root, next)
			info, err := os.Lstat(containerNext)
			if os.IsNotExist(err) {
				return hostPathToContainer(root, current)
			}
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				prefix = next
				continue
			}
			target, err := os.Readlink(containerNext)
			if err != nil {
				return hostPathToContainer(root, current)
			}
			remaining := filepath.Join(parts[i+1:]...)
			if filepath.IsAbs(target) {
				current = filepath.Join(target, remaining)
			} else {
				current = filepath.Join(filepath.Dir(next), target, remaining)
			}
			if !filepath.IsAbs(current) {
				current = string(filepath.Separator) + current
			}
			goto resolvedOneSymlink
		}
		return hostPathToContainer(root, current)
	resolvedOneSymlink:
	}
	return hostPathToContainer(root, current)
}

func decide(node Node) Decision {
	hostRoot := env("HOST_ROOT", defaultHostRoot)
	dataDir := resolveHostPath(hostRoot, env("HOST_PXC_DATA_DIR", defaultHostPXCDataDir))
	minRAM := envInt64("PXC_MIN_RAM_MB", defaultMinRAMMB)
	minFree := envInt64("PXC_MIN_FREE_BYTES", defaultMinFreeBytes)
	dbBytes := readClusterDBBytes()
	requiredFree := minFree
	if dbBytes*2 > requiredFree {
		requiredFree = dbBytes * 2
	}

	intent := "auto"
	if value, ok, err := consulRawKey("pxc/nodes/" + node.Name + "/intent"); err == nil && ok && value != "" {
		intent = strings.ToLower(value)
	}
	if metaIntent := node.Meta["pxc_intent"]; metaIntent != "" && intent == "auto" {
		intent = strings.ToLower(metaIntent)
	}

	dataDirExists := false
	if info, err := os.Stat(dataDir); err == nil && info.IsDir() {
		dataDirExists = true
	}
	ram := memoryMB(node)
	free := statFree(dataDir)
	dbLocal := int64(0)
	if dataDirExists {
		dbLocal = dirSize(dataDir)
	}
	member := dataDirExists && existingMember(dataDir)
	consulServer := node.Attributes["consul.server"] == "true"
	dockerHealthy := node.Attributes["driver.docker"] == "1" || node.Attributes["driver.docker"] == "true"

	decision := Decision{
		NodeName:         node.Name,
		NodeID:           node.ID,
		Intent:           intent,
		MemberRole:       "none",
		Admission:        "blocked",
		Reason:           "not-classified",
		StorageStatus:    "unknown",
		RAMMB:            ram,
		StorageFreeBytes: free,
		DBBytes:          dbBytes,
		DataDirExists:    dataDirExists,
		ExistingMember:   member,
		ConsulServer:     consulServer,
		DockerHealthy:    dockerHealthy,
	}
	if dbBytes == 0 && dbLocal > 0 {
		decision.DBBytes = dbLocal
	}
	if free >= requiredFree {
		decision.StorageStatus = "ok"
	} else {
		decision.StorageStatus = "warn"
	}
	if !consulServer || !dockerHealthy {
		decision.Reason = "missing-consul-server-or-docker"
		return decision
	}
	switch intent {
	case "disabled":
		decision.MemberRole = "none"
		decision.Admission = "blocked"
		decision.Reason = "intent-disabled"
	case "arbiter":
		decision.MemberRole = "arbiter"
		decision.Admission = "allowed"
		decision.Reason = "intent-arbiter"
	case "data":
		decision.MemberRole = "data"
		decision.Admission = "manual"
		decision.Reason = "intent-data"
	case "auto", "":
		if member {
			decision.MemberRole = "data"
			decision.Admission = "allowed"
			decision.Reason = "existing-data-member"
		} else if ram >= minRAM && dataDirExists && free >= requiredFree {
			decision.MemberRole = "data"
			decision.Admission = "allowed"
			decision.Reason = "auto-data"
		} else if ram < minRAM {
			decision.MemberRole = "arbiter"
			decision.Admission = "allowed"
			decision.Reason = "auto-arbiter"
		} else if !dataDirExists {
			decision.MemberRole = "none"
			decision.Admission = "blocked"
			decision.Reason = "missing-data-dir"
		} else {
			decision.MemberRole = "none"
			decision.Admission = "blocked"
			decision.Reason = "insufficient-storage"
		}
	default:
		decision.MemberRole = "none"
		decision.Admission = "blocked"
		decision.Reason = "invalid-intent"
	}
	if decision.MemberRole == "data" && decision.StorageStatus != "ok" {
		decision.Reason += "-storage-warn"
	}
	return decision
}

func readClusterDBBytes() int64 {
	value, ok, err := consulRawKey("pxc/cluster/db_bytes")
	if err != nil || !ok {
		return 0
	}
	parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return parsed
}

func applyMeta(decision Decision) error {
	args := []string{
		"node", "meta", "apply",
		"-address=" + nomadBase(),
		"-region=" + env("NOMAD_REGION", "global"),
		"-node-id=" + decision.NodeID,
		"pxc_intent=" + decision.Intent,
		"pxc_member_role=" + decision.MemberRole,
		"pxc_admission=" + decision.Admission,
		"pxc_storage_status=" + decision.StorageStatus,
		"pxc_reason=" + decision.Reason,
		fmt.Sprintf("pxc_ram_mb=%d", decision.RAMMB),
		fmt.Sprintf("pxc_storage_free_bytes=%d", decision.StorageFreeBytes),
		fmt.Sprintf("pxc_db_bytes=%d", decision.DBBytes),
		fmt.Sprintf("pxc_existing_member=%t", decision.ExistingMember),
	}
	cmd := exec.Command("nomad", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nomad node meta apply failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func runOnce(apply bool) error {
	current, err := node()
	if err != nil {
		return err
	}
	decision := decide(current)
	encoded, _ := json.Marshal(decision)
	fmt.Println(string(encoded))
	if apply {
		if err := applyMeta(decision); err != nil {
			return err
		}
		if err := putConsulJSON("pxc/node-classifier/"+decision.NodeName, decision); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	apply := env("APPLY", "1") == "1"
	once := env("ONCE", "0") == "1"
	if err := runOnce(apply); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if once {
		return
	}
	interval, err := time.ParseDuration(env("INTERVAL", defaultInterval.String()))
	if err != nil {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	for range ticker.C {
		if err := runOnce(apply); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cliFixture(t *testing.T) (root, db string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("DIRSPACE_LOCK_DIR", filepath.Join(base, "locks"))
	root = filepath.Join(base, "root")
	db = filepath.Join(base, "db", "usage.db")
	if err := os.MkdirAll(filepath.Join(root, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "beta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "alpha", "a"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "beta", "b"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, db
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := ExecuteContext(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCLIRejectsExplicitDBInsideRoot(t *testing.T) {
	root, _ := cliFixture(t)
	inside := filepath.Join(root, "usage.db")
	code, stdout, stderr := runCLI(t, "--db", inside, "scan", "--path", root)
	if code == 0 || !strings.Contains(stderr, "数据库不能位于扫描根目录内") {
		t.Fatalf("expected guard, exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("guard should run before opening the database: %v", err)
	}
}

func TestCLIEndToEnd(t *testing.T) {
	root, db := cliFixture(t)
	code, stdout, stderr := runCLI(t, "--db", db, "scan", "--path", root)
	if code != 0 {
		t.Fatalf("scan exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "扫描结果") {
		t.Fatalf("missing scan output: %s", stdout)
	}

	code, stdout, stderr = runCLI(t, "--db", db, "--path", root, "list", "--json")
	if code != 0 {
		t.Fatalf("list exit=%d stderr=%s", code, stderr)
	}
	var output map[string]any
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("bad json: %v\n%s", err, stdout)
	}
	dirs, ok := output["directories"].([]any)
	if !ok || len(dirs) != 2 {
		t.Fatalf("directories=%v", output["directories"])
	}
	if output["size_basis"] != "allocated" {
		t.Fatalf("basis=%v", output["size_basis"])
	}

	code, stdout, _ = runCLI(t, "--db", db, "--path", root, "top", "1")
	if code != 0 || !strings.Contains(stdout, "alpha") || strings.Contains(stdout, "beta") {
		t.Fatalf("top output wrong: %s", stdout)
	}

	code, stdout, stderr = runCLI(t, "--db", db, "--path", root, "children", "--json")
	if code != 0 {
		t.Fatalf("children exit=%d stderr=%s", code, stderr)
	}
	output = nil
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("children json: %v\n%s", err, stdout)
	}
	children, ok := output["children"].([]any)
	if !ok || len(children) != 2 {
		t.Fatalf("children=%v", output["children"])
	}

	code, stdout, stderr = runCLI(t, "--db", db, "--path", root, "size", "--json")
	if code != 0 {
		t.Fatalf("size exit=%d stderr=%s", code, stderr)
	}
	output = nil
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("size json: %v\n%s", err, stdout)
	}
	if output["size_bytes"] == nil || output["source"] != "root" {
		t.Fatalf("size output=%v", output)
	}
	code, stdout, _ = runCLI(t, "--db", db, "--path", filepath.Join(root, "alpha"), "size")
	if code != 0 || !strings.Contains(stdout, "目录大小") {
		t.Fatalf("child size: exit=%d stdout=%s", code, stdout)
	}

	code, stdout, _ = runCLI(t, "--db", db, "--path", root, "status", "--json")
	if code != 0 {
		t.Fatalf("status exit=%d", code)
	}
	output = nil
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("status json: %v", err)
	}
	if output["data_status"] != "fresh" {
		t.Fatalf("status=%v", output["data_status"])
	}
	if _, ok := output["metadata_qps"]; !ok {
		t.Fatalf("status missing metadata_qps: %v", output)
	}
	if _, ok := output["metadata_qps_limit"]; !ok {
		t.Fatalf("status missing metadata_qps_limit: %v", output)
	}
	code, stdout, _ = runCLI(t, "--db", db, "--path", root, "status")
	if code != 0 || !strings.Contains(stdout, "QPS") {
		t.Fatalf("status text missing QPS: %s", stdout)
	}

	code, stdout, _ = runCLI(t, "--db", db, "rebuild", "--path", filepath.Join(root, "alpha"))
	if code != 0 || !strings.Contains(stdout, "单目录重算") {
		t.Fatalf("rebuild failed: %s", stdout)
	}

	// Explicit root plus a directory name must not be mistaken for a target
	// child path by the database-location guard.
	code, stdout, stderr = runCLI(t, "--db", db, "rebuild", "alpha", "--path", root)
	if code != 0 || !strings.Contains(stdout, "单目录重算") {
		t.Fatalf("rebuild with explicit root failed: exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}

	backup := filepath.Join(filepath.Dir(db), "backup.db")
	code, stdout, _ = runCLI(t, "--db", db, "backup", "--output", backup)
	if code != 0 {
		t.Fatalf("backup failed: %s", stdout)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}

	code, stdout, _ = runCLI(t, "--db", db, "doctor")
	if code != 0 || !strings.Contains(stdout, "通过") {
		t.Fatalf("doctor failed: %s", stdout)
	}
}

func TestCLIUsageErrors(t *testing.T) {
	code, _, _ := runCLI(t, "--not-a-flag")
	if code != exitUsage {
		t.Fatalf("exit=%d", code)
	}
	code, _, _ = runCLI(t, "top", "abc")
	if code != exitUsage {
		t.Fatalf("top exit=%d", code)
	}
	root, db := cliFixture(t)
	code, _, _ = runCLI(t, "--db", db, "--path", root, "pause")
	if code != exitRuntime {
		t.Fatalf("pause exit=%d", code)
	}
	code, _, _ = runCLI(t, "--staging-only", "status")
	if code != exitUsage {
		t.Fatalf("staging-only on status exit=%d", code)
	}
	code, _, _ = runCLI(t, "--exact", "status")
	if code != exitUsage {
		t.Fatalf("exact on status exit=%d", code)
	}
}

func TestCLIVersionHelp(t *testing.T) {
	code, stdout, _ := runCLI(t, "--version")
	if code != 0 || !strings.Contains(stdout, Version) {
		t.Fatalf("version: %s", stdout)
	}
	code, stdout, _ = runCLI(t, "--help")
	if code != 0 || !strings.Contains(stdout, "dirspace") {
		t.Fatalf("help: %s", stdout)
	}
}

func TestConfigExplainWorksWithoutDatabase(t *testing.T) {
	root, db := cliFixture(t)
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(filepath.Dir(db), "config.yaml")
	content := "profile: low\n" +
		"database:\n  path: " + db + "\n" +
		"scan:\n  root_path: " + root + "\n  interval: 1h\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "--config", cfgPath, "config", "explain")
	if code != 0 {
		t.Fatalf("config exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "profile：low") || !strings.Contains(stdout, "auto (max 4)") {
		t.Fatalf("config output: %s", stdout)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("config explain opened the database: %v", err)
	}
}

func TestCLIShardAndMerge(t *testing.T) {
	root, _ := cliFixture(t)
	base := filepath.Dir(root)
	shard0 := filepath.Join(base, "db", "shard0.db")
	shard1 := filepath.Join(base, "db", "shard1.db")
	merged := filepath.Join(base, "db", "merged.db")

	for _, tc := range []struct {
		db    string
		shard string
	}{{shard0, "0/2"}, {shard1, "1/2"}} {
		code, stdout, stderr := runCLI(t, "--db", tc.db, "scan", "--path", root, "--shard", tc.shard)
		if code != 0 {
			t.Fatalf("shard %s exit=%d stdout=%s stderr=%s", tc.shard, code, stdout, stderr)
		}
		if !strings.Contains(stdout, "分片") {
			t.Fatalf("shard %s missing note: %s", tc.shard, stdout)
		}
	}

	code, stdout, stderr := runCLI(t, "--db", merged, "--path", root,
		"merge", "--inputs", shard0+","+shard1)
	if code != 0 {
		t.Fatalf("merge exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "合并完成") {
		t.Fatalf("merge output: %s", stdout)
	}

	code, stdout, stderr = runCLI(t, "--db", merged, "--path", root, "children", "--json")
	if code != 0 {
		t.Fatalf("merged children exit=%d stderr=%s", code, stderr)
	}
	var output map[string]any
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("children json: %v", err)
	}
	children, ok := output["children"].([]any)
	if !ok || len(children) != 2 {
		t.Fatalf("merged children=%v", output["children"])
	}
	code, stdout, _ = runCLI(t, "--db", merged, "--path", root, "size", "--json")
	if code != 0 {
		t.Fatalf("merged size exit=%d stdout=%s", code, stdout)
	}
	output = nil
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("size json: %v", err)
	}
	if output["size_bytes"].(float64) <= 0 {
		t.Fatalf("merged size=%v", output)
	}
}

func TestCLIFSUsage(t *testing.T) {
	root, db := cliFixture(t)
	code, stdout, stderr := runCLI(t, "--db", db, "--path", root, "fs-usage", "--json")
	if code != 0 {
		t.Fatalf("fs-usage exit=%d stderr=%s", code, stderr)
	}
	var output map[string]any
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout)
	}
	if output["total_bytes"].(float64) <= 0 {
		t.Fatalf("fs usage=%v", output)
	}
}

func TestCLIMonitor(t *testing.T) {
	root, db := cliFixture(t)
	code, stdout, stderr := runCLI(t, "--db", db, "--path", root, "monitor")
	if code != 0 {
		t.Fatalf("monitor exit=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"资源与配置体检", "配置", "运行", "判断", "结论"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("monitor output missing %q:\n%s", want, stdout)
		}
	}
}

func TestAssessMonitorFlagsLowAndHigh(t *testing.T) {
	low := assessMonitor(monitorReport{
		Config: monitorConfig{MaxCPUPercent: 20, MaxMetadataQPS: 300},
		Runtime: monitorRuntime{
			Active: true, ProcessCPUPercent: 19, QPS: 100, QPSLimit: 300,
			HostCPUPercent: 20,
		},
	})
	if low.Level != "low" {
		t.Fatalf("expected low, got %+v", low)
	}
	high := assessMonitor(monitorReport{
		Config: monitorConfig{MaxHostCPUPercent: 85, MaxMetadataQPS: 300},
		Runtime: monitorRuntime{
			Active: true, HostCPUPercent: 95, QPS: 100, QPSLimit: 300,
		},
	})
	if high.Level != "high" {
		t.Fatalf("expected high, got %+v", high)
	}
}

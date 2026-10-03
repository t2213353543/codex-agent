package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCodexCLIWorkloadFixture executes the unmodified official CLI through the
// real workload harness and gateway. The upstream supplies deterministic code,
// not model inference. No real account or synthetic cache-hit claim is involved.
func TestCodexCLIWorkloadFixture(t *testing.T) {
	bin := os.Getenv("CODEX_REAL_CLI_BIN")
	if bin == "" {
		t.Skip("set CODEX_REAL_CLI_BIN for official CLI workload harness integration")
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python")
	if err != nil {
		t.Fatal("Python is required to execute the workload harness")
	}
	script, err := filepath.Abs(filepath.Join(projectRoot(t), "tools", "codex_cli_workload.py"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var count, observedToolResults int
	var marker, session string
	markerPattern := regexp.MustCompile(`ledger_[0-9a-f]{32}`)
	p := newTestProxy(t, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer test-upstream-token" {
			t.Error("workload request did not use the isolated local response fixture")
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&request); err != nil {
			t.Error("cannot decode fixture request")
			http.Error(w, "invalid fixture JSON", http.StatusBadRequest)
			return
		}
		count++
		input := realCLIJSON(request["input"])
		if count == 1 {
			marker = markerPattern.FindString(input)
			session = r.Header.Get("Session-Id")
			if marker == "" || session == "" {
				t.Error("initial request lacked conversation marker or native session identity")
			}
		} else if r.Header.Get("Session-Id") != session {
			t.Error("resumed CLI changed session identity")
		}
		var item map[string]any
		switch count {
		case 1, 3:
			name, namespace := realCLIShellTool(realCLIArray(request["tools"]), "")
			if name == "" {
				t.Error("CLI did not advertise an executable shell tool")
				http.Error(w, "missing shell tool", http.StatusBadRequest)
				return
			}
			command := workloadFixtureCommand(count == 3)
			args := map[string]any{"cmd": command, "login": false, "max_output_tokens": 2000, "yield_time_ms": 10000}
			if name == "shell_command" {
				args = map[string]any{"command": command, "login": false, "timeout_ms": 20000}
			} else if name == "shell" {
				argv := []string{"sh", "-c", command}
				if runtime.GOOS == "windows" {
					argv = []string{"powershell.exe", "-NoProfile", "-Command", command}
				}
				args = map[string]any{"command": argv, "timeout_ms": 20000}
			}
			item = map[string]any{"id": fmt.Sprintf("fc_workload_%d", count), "type": "function_call", "call_id": fmt.Sprintf("call_workload_%d", count), "name": name, "arguments": realCLIJSON(args)}
			if namespace != "" {
				item["namespace"] = namespace
			}
		case 2, 4:
			wantCall := fmt.Sprintf("call_workload_%d", count-1)
			for _, value := range realCLIArray(request["input"]) {
				result, _ := value.(map[string]any)
				if result["type"] != "function_call_output" || result["call_id"] != wantCall {
					continue
				}
				output := realCLIJSON(result["output"])
				if !strings.Contains(output, "Ran ") || !strings.Contains(output, "OK") {
					t.Errorf("real shell did not complete unittest: %s", output[max(0, len(output)-1200):])
				} else {
					observedToolResults++
				}
			}
			item = realCLIMessage(fmt.Sprintf("msg_workload_%d", count), "Fixture implementation updated and unittest passed.")
		case 5:
			if marker == "" || !strings.Contains(input, marker) {
				t.Error("native resume did not preserve the first-turn marker in history")
			}
			item = realCLIMessage("msg_workload_memory", marker)
		default:
			t.Errorf("unexpected extra fixture model request %d", count)
			http.Error(w, "unexpected extra response", http.StatusBadRequest)
			return
		}
		// This existing helper reports no cached_tokens, so the fixture cannot
		// accidentally be presented as evidence of a real cache hit.
		realCLIWriteSSE(w, fmt.Sprintf("resp_workload_%d", count), item, 10)
	}))
	t.Cleanup(upstream.Close)
	p.BaseURL, p.Client = upstream.URL, upstream.Client()
	gateway := httptest.NewServer(p)
	t.Cleanup(gateway.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, script, "--base-url", gateway.URL+"/v1", "--model", "gpt-5.4", "--codex", bin, "--timeout", "45")
	for _, name := range []string{"PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "TEMP", "TMP", "USERPROFILE", "HOME", "APPDATA", "LOCALAPPDATA", "PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMDATA", "SHELL"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_GATEWAY_API_KEY=test-client-key", "NO_PROXY=*", "no_proxy=*", "PYTHONDONTWRITEBYTECODE=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var report struct {
		Passed     bool   `json:"passed"`
		CLIVersion string `json:"cli_version"`
		Checks     []struct {
			Name       string          `json:"name"`
			Passed     bool            `json:"passed"`
			Assertions map[string]bool `json:"assertions"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("workload did not emit JSON: process=%v stdout=%s stderr=%s", runErr, &stdout, &stderr)
	}
	if runErr != nil || !report.Passed || len(report.Checks) != 3 {
		t.Fatalf("official CLI workload harness failed: process=%v summary=%s stderr=%s", runErr, &stdout, &stderr)
	}
	for _, check := range report.Checks {
		if !check.Passed {
			t.Fatalf("workload check failed: %s", check.Name)
		}
		for name, passed := range check.Assertions {
			if !passed {
				t.Fatalf("workload assertion failed: %s/%s", check.Name, name)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 5 || observedToolResults != 2 {
		t.Fatalf("unexpected actual CLI tool execution: responses=%d unittest_outputs=%d", count, observedToolResults)
	}
	t.Logf("%s: workspace-write/never, native unelevated Windows backend when applicable; repair + monthly feature + persisted resume passed using 2 real shell commands and independent money/UTC-month oracles; deterministic local fixture, not model inference or cache measurement", report.CLIVersion)
}

func workloadFixtureCommand(monthly bool) string {
	ledger := `from datetime import datetime, timezone
from decimal import Decimal, ROUND_HALF_UP

def totals(rows):
    result = {}
    for row in rows:
        amount = Decimal(row["amount"]).quantize(Decimal("0.01"), rounding=ROUND_HALF_UP)
        currency = row["currency"]
        result[currency] = result.get(currency, Decimal(0)) + amount
    return {key: format(value, ".2f") for key, value in result.items()}
`
	extra := `import copy
import unittest
from ledger import totals

class ExtraLedgerTests(unittest.TestCase):
    def test_refunds_zero_and_no_mutation(self):
        rows = [{"currency": "USD", "amount": "1.005"}, {"currency": "USD", "amount": "-1.005"}]
        original = copy.deepcopy(rows)
        self.assertEqual(totals(rows), {"USD": "0.00"})
        self.assertEqual(rows, original)
`
	if monthly {
		ledger += `
def monthly_totals(rows):
    months = {}
    for row in rows:
        timestamp = datetime.fromisoformat(row["timestamp"].replace("Z", "+00:00"))
        month = timestamp.astimezone(timezone.utc).strftime("%Y-%m")
        months.setdefault(month, []).append(row)
    return {month: totals(group) for month, group in months.items()}
`
		extra += `
    def test_utc_boundaries_and_no_mutation(self):
        from ledger import monthly_totals
        rows = [{"currency": "USD", "amount": "0.105", "timestamp": "2026-03-01T00:20:00+02:00"},
                {"currency": "EUR", "amount": "-1.235", "timestamp": "2026-02-28T23:50:00-03:00"}]
        original = copy.deepcopy(rows)
        self.assertEqual(monthly_totals(rows), {"2026-02": {"USD": "0.11"}, "2026-03": {"EUR": "-1.24"}})
        self.assertEqual(monthly_totals([]), {})
        self.assertEqual(rows, original)
`
	}
	// Base64 avoids shell interpolation of Python source while all writes stay
	// in the CLI's disposable workspace. unittest itself remains a real command.
	program := "from pathlib import Path\nPath('ledger.py').write_text(" + realCLIJSON(ledger) + ", encoding='utf-8')\nPath('test_extra.py').write_text(" + realCLIJSON(extra) + ", encoding='utf-8')\n"
	command := `python -c "import base64;exec(base64.b64decode('` + base64.StdEncoding.EncodeToString([]byte(program)) + `'))"`
	if runtime.GOOS == "windows" {
		return command + "; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; python -m unittest discover -v"
	}
	return command + " && python -m unittest discover -v"
}

package k8s

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stearz/voidshell/internal/server"
	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// TestBackendBenchmark runs only inside an explicitly provisioned disposable
// cluster. Its test-only authenticator accepts generated keys; never deploy this
// test runner in an existing cluster. It needs a namespace and API metrics read.
func TestBackendBenchmark(t *testing.T) {
	image := os.Getenv("VOIDSHELL_BENCHMARK_IMAGE")
	if image == "" {
		t.Skip("set VOIDSHELL_BENCHMARK_IMAGE in a disposable cluster")
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		t.Fatal(err)
	}
	api, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := t.TempDir()
	key, _, err := generateSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "large")
	large := bytes.Repeat([]byte("VoidShell benchmark payload 0123456789abcdef\n"), 400000)[:16*1024*1024]
	if err := os.WriteFile(source, large, 0600); err != nil {
		t.Fatal(err)
	}
	smallDir := filepath.Join(dir, "small")
	if err := os.Mkdir(smallDir, 0700); err != nil {
		t.Fatal(err)
	}
	small := bytes.Repeat([]byte("x"), 4096)
	for i := range 200 {
		if err := os.WriteFile(filepath.Join(smallDir, fmt.Sprintf("file-%04d", i)), small, 0600); err != nil {
			t.Fatal(err)
		}
	}
	metrics := func() map[string]float64 {
		t.Helper()
		raw, err := api.CoreV1().RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
		if err != nil {
			t.Fatal(err)
		}
		result := map[string]float64{}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			if fields[0] == "process_cpu_seconds_total" || fields[0] == "process_resident_memory_bytes" {
				value, err := strconv.ParseFloat(fields[1], 64)
				if err != nil {
					t.Fatal(err)
				}
				result[fields[0]] = value
			}
		}
		if len(result) != 2 {
			t.Fatal("API server CPU/RSS metrics unavailable")
		}
		return result
	}
	for _, backend := range []string{"kubernetes", "ssh"} {
		t.Run(backend, func(t *testing.T) {
			m := New(api, Config{Namespace: "voidshell-benchmark", Backend: backend, Image: image, PodReadyTimeout: 90 * time.Second})
			m.restCfg = cfg
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			runCtx, stop := context.WithCancel(ctx)
			srv := server.New(signer, integrationAuth{}, m, m, slog.New(slog.NewTextHandler(os.Stderr, nil)))
			done := make(chan error, 1)
			go func() { done <- srv.Serve(runCtx, ln) }()
			defer func() {
				stop()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{User: "benchmark", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(signer.PublicKey())})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			keeper, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			defer keeper.Close()
			keeper.Stderr = os.Stderr
			if _, err := keeper.StdinPipe(); err != nil {
				t.Fatal(err)
			}
			output, err := keeper.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := keeper.Start("printf 'ready\\n'; cat"); err != nil {
				t.Fatal(err)
			}
			if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("keeper ready: %q %v", line, err)
			}
			port := ln.Addr().(*net.TCPAddr).Port
			known := filepath.Join(dir, backend+"-hosts")
			if err := os.WriteFile(known, []byte(fmt.Sprintf("[127.0.0.1]:%d %s", port, ssh.MarshalAuthorizedKey(signer.PublicKey()))), 0600); err != nil {
				t.Fatal(err)
			}
			before := metrics()
			time.Sleep(2 * time.Second)
			after := metrics()
			record := func(data map[string]any) {
				raw, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				fmt.Printf("BENCHMARK %s\n", raw)
			}
			record(map[string]any{"backend": backend, "workload": "idle", "seconds": 2, "api_cpu_seconds": after["process_cpu_seconds_total"] - before["process_cpu_seconds_total"], "api_rss_bytes": after["process_resident_memory_bytes"]})
			for _, workload := range []string{"large", "small"} {
				for _, concurrency := range []int{1, 4, 16} {
					for trial := range 3 {
						var group sync.WaitGroup
						failures := make(chan error, concurrency)
						before := metrics()
						start := time.Now()
						for worker := range concurrency {
							group.Add(1)
							go func() {
								defer group.Done()
								remote := fmt.Sprintf("/home/workspace/bench-%s-%d-%d-%d", workload, concurrency, trial, worker)
								download := filepath.Join(dir, fmt.Sprintf("download-%s-%s-%d-%d-%d", backend, workload, concurrency, trial, worker))
								batch := "mkdir " + remote + "\n"
								if workload == "large" {
									batch += fmt.Sprintf("put %s %s/payload\nget %s/payload %s\nrm %s/payload\nrmdir %s\n", source, remote, remote, download, remote, remote)
								} else {
									batch += fmt.Sprintf("put -r %s %s/small\nget -r %s/small %s\n", smallDir, remote, remote, download)
								}
								cmd := exec.CommandContext(ctx, "sftp", "-q", "-b", "-", "-P", strconv.Itoa(port), "-i", keyPath, "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+known, "benchmark@127.0.0.1")
								cmd.Stdin = strings.NewReader(batch)
								if out, err := cmd.CombinedOutput(); err != nil {
									failures <- fmt.Errorf("sftp: %w: %s", err, out)
									return
								}
								if workload == "large" {
									data, err := os.ReadFile(download)
									if err != nil || !bytes.Equal(data, large) {
										failures <- fmt.Errorf("large round trip differs: %v", err)
									}
								} else {
									for i := range 200 {
										data, err := os.ReadFile(filepath.Join(download, fmt.Sprintf("file-%04d", i)))
										if err != nil || !bytes.Equal(data, small) {
											failures <- fmt.Errorf("small round trip differs: %v", err)
											break
										}
									}
								}
								os.RemoveAll(download)
							}()
						}
						group.Wait()
						elapsed := time.Since(start).Seconds()
						after := metrics()
						close(failures)
						for err := range failures {
							t.Fatal(err)
						}
						size := int64(len(large))
						if workload == "small" {
							size = 200 * 4096
						}
						total := size * int64(concurrency) * 2
						record(map[string]any{"backend": backend, "workload": workload, "concurrency": concurrency, "trial": trial + 1, "bytes": total, "seconds": elapsed, "mib_per_second": float64(total) / 1048576 / elapsed, "api_cpu_seconds": after["process_cpu_seconds_total"] - before["process_cpu_seconds_total"], "api_rss_bytes": after["process_resident_memory_bytes"]})
					}
				}
			}
		})
	}
}

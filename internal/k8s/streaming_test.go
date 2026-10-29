package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	streamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/stearz/voidshell/internal/workspace"
)

func TestExecutionResult(t *testing.T) {
	infra := errors.New("connection lost")
	for _, tt := range []struct {
		name    string
		err     error
		code    uint32
		wantErr bool
	}{
		{"success", nil, 0, false},
		{"remote exit", utilexec.CodeExitError{Err: errors.New("exit"), Code: 23}, 23, false},
		{"wrapped exit", fmt.Errorf("wrapped: %w", utilexec.CodeExitError{Err: errors.New("exit"), Code: 127}), 127, false},
		{"infrastructure", infra, 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code, err := executionResult(tt.err)
			if code != tt.code || (err != nil) != tt.wantErr {
				t.Fatalf("result = %d, %v", code, err)
			}
		})
	}
}

func managerForHTTP(t *testing.T, handler http.HandlerFunc) *Manager {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := &rest.Config{Host: server.URL}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := New(client, testConfig())
	m.restCfg = cfg
	return m
}

func TestExecRequestEncodesArgvAndStreams(t *testing.T) {
	requests := make(chan *http.Request, 2)
	m := managerForHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		http.Error(w, "test endpoint", http.StatusBadRequest)
	})
	id := workspace.New("alice", "dev")
	for _, tty := range []bool{false, true} {
		_, err := m.Exec(context.Background(), id, workspace.StorageEphemeral, []string{"/bin/sh", "-c", `printf '%s' 'a b'`}, tty, nil, strings.NewReader(""), io.Discard, io.Discard)
		if err == nil {
			t.Fatal("HTTP rejection must be an infrastructure error")
		}
		req := <-requests
		if req.URL.Path != "/api/v1/namespaces/test-ns/pods/"+id.PodNameFor(workspace.StorageEphemeral)+"/exec" {
			t.Fatalf("path = %s", req.URL.Path)
		}
		q := req.URL.Query()
		if argv := q["command"]; len(argv) != 3 || argv[2] != `printf '%s' 'a b'` {
			t.Fatalf("argv = %q", argv)
		}
		if q.Get("stdin") != "true" || q.Get("stdout") != "true" || (q.Get("stderr") == "true") == tty || (q.Get("tty") == "true") != tty {
			t.Fatalf("stream flags = %v", q)
		}
	}
}

func TestPortForwardSPDYLoopbackAndHalfClose(t *testing.T) {
	// Speak the kubelet port-forward protocol over an actual upgraded HTTP
	// connection. The response is sent only after input EOF, testing half-close.
	paths := make(chan string, 1)
	handlerErr := make(chan error, 1)
	m := managerForHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		if _, err := httpstream.Handshake(r, w, []string{portforward.PortForwardProtocolV1Name}); err != nil {
			handlerErr <- err
			return
		}
		streams := make(chan httpstream.Stream, 2)
		conn := streamspdy.NewResponseUpgrader().UpgradeResponse(w, r, func(s httpstream.Stream, replySent <-chan struct{}) error {
			go func() { <-replySent; streams <- s }()
			return nil
		})
		if conn == nil {
			handlerErr <- errors.New("upgrade failed")
			return
		}
		defer conn.Close()
		var data, errorStream httpstream.Stream
		for range 2 {
			select {
			case s := <-streams:
				if s.Headers().Get(corev1.PortHeader) != "3000" {
					handlerErr <- fmt.Errorf("port = %s", s.Headers().Get(corev1.PortHeader))
					return
				}
				switch s.Headers().Get(corev1.StreamType) {
				case corev1.StreamTypeError:
					errorStream = s
				case corev1.StreamTypeData:
					data = s
				}
			case <-time.After(5 * time.Second):
				handlerErr <- errors.New("stream setup timeout")
				return
			}
		}
		if data == nil || errorStream == nil {
			handlerErr <- errors.New("missing stream pair")
			return
		}
		payload, err := io.ReadAll(data)
		if err == nil {
			_, err = data.Write(append([]byte("response:"), payload...))
		}
		data.Close()
		errorStream.Close()
		handlerErr <- err
		<-conn.CloseChan()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	id := workspace.New("alice", "dev")
	if err := m.Forward(ctx, id, workspace.StoragePersistent, 3000, strings.NewReader("hello"), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "response:hello" {
		t.Fatalf("output=%q", out.String())
	}
	if path := <-paths; path != "/api/v1/namespaces/test-ns/pods/"+id.PodNameFor(workspace.StoragePersistent)+"/portforward" {
		t.Fatalf("path=%s", path)
	}
	if err := <-handlerErr; err != nil {
		t.Fatal(err)
	}
}

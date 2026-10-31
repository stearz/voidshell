#!/usr/bin/env bash
# Run only in a new disposable kind cluster. Never uses the current kubeconfig.
set -euo pipefail
cd "$(dirname "$0")/.."
output=${1:-backend-benchmark.log}
case "$output" in /*) ;; *) output="$PWD/$output" ;; esac
work=$(mktemp -d)
cluster="voidshell-benchmark-$$"
created=false
cleanup() {
  if "$created"; then kind delete cluster --name "$cluster" >/dev/null; fi
  rm -rf "$work"
}
trap cleanup EXIT
arch=$(docker version --format '{{.Server.Arch}}')
GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go test -c -o "$work/k8s.test" ./internal/k8s
docker build -f internal/k8s/testdata/Dockerfile.ssh-test -t voidshell-benchmark-workspace:test .
cat > "$work/Dockerfile" <<'DOCKERFILE'
FROM voidshell-benchmark-workspace:test
USER root
COPY k8s.test /usr/local/bin/k8s.test
ENTRYPOINT ["/usr/local/bin/k8s.test"]
DOCKERFILE
docker build -t voidshell-benchmark-runner:test "$work"
created=true
kind create cluster --name "$cluster" --kubeconfig "$work/kubeconfig" --wait 60s
kind load docker-image voidshell-benchmark-workspace:test voidshell-benchmark-runner:test --name "$cluster"
kubectl --kubeconfig "$work/kubeconfig" --context "kind-$cluster" apply -f internal/k8s/testdata/benchmark.yaml
kubectl --kubeconfig "$work/kubeconfig" --context "kind-$cluster" -n voidshell-benchmark wait --for=condition=Ready pod/benchmark --timeout=90s
kubectl --kubeconfig "$work/kubeconfig" --context "kind-$cluster" -n voidshell-benchmark logs -f benchmark --pod-running-timeout=90s | tee "$output"
phase=$(kubectl --kubeconfig "$work/kubeconfig" --context "kind-$cluster" -n voidshell-benchmark get pod benchmark -o jsonpath='{.status.phase}')
# The log stream can close before kubelet publishes the terminal status.
if [ "$phase" != Succeeded ]; then
  kubectl --kubeconfig "$work/kubeconfig" --context "kind-$cluster" -n voidshell-benchmark wait --for=jsonpath='{.status.phase}'=Succeeded pod/benchmark --timeout=15s
fi
printf 'Benchmark log: %s\n' "$output"

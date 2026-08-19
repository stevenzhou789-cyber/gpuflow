set -eu

: "${GPUFLOW_ARTIFACT_DIR:?GPUFLOW_ARTIFACT_DIR is required}"

echo "GPUFLOW_QUICK_SMOKE_OK"
echo "host=$(hostname)"
echo "started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
uname -a

artifact_dir="$GPUFLOW_ARTIFACT_DIR"
mkdir -p "$artifact_dir"
{
  echo "status=ok"
  echo "host=$(hostname)"
  echo "finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} > "$artifact_dir/result.txt"
test -s "$artifact_dir/result.txt"
echo "artifact=$artifact_dir/result.txt"

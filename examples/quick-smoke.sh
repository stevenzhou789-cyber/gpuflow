set -eu

echo "GPUFLOW_QUICK_SMOKE_OK"
echo "host=$(hostname)"
echo "started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
uname -a

artifact_dir="${GPUFLOW_ARTIFACT_DIR:-}"
if [ -n "$artifact_dir" ]; then
  mkdir -p "$artifact_dir"
  {
    echo "status=ok"
    echo "host=$(hostname)"
    echo "finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } > "$artifact_dir/result.txt"
  echo "artifact=$artifact_dir/result.txt"
fi

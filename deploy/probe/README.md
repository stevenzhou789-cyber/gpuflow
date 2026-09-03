# GPU probe image

GPUFlow uses this image only to run the host-provided `nvidia-smi` inside an
NVIDIA-enabled container. It is deliberately separate from the Alpine-based
Agent image because the NVIDIA driver binary requires a glibc dynamic loader.

Build and publish both supported Linux architectures:

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t registry.example.com/gpuflow/gpu-probe:v1.0.18 \
  --push deploy/probe
```

Air-gapped builds may override the base with a preloaded glibc image by passing
`--build-arg PROBE_BASE_IMAGE=<local-image>`.

The default Debian base is pinned to a multi-architecture Digest. Commercial
release pipelines must keep the override immutable as well and scan the final
Probe image before signing it.

The control plane advertises the configured image to every Agent. Online Docker
nodes pull it automatically when missing; air-gapped nodes must preload the same
immutable image before the Agent starts.

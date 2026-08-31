ARG PROBE_BASE_IMAGE=debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171
FROM ${PROBE_BASE_IMAGE}

ENV NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=compute,utility

LABEL org.opencontainers.image.title="GPUFlow GPU Probe" \
      org.opencontainers.image.description="glibc runtime used by GPUFlow agents to validate NVIDIA container access"

ENTRYPOINT ["nvidia-smi"]

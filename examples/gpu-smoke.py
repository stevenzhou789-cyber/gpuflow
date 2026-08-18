import time

import torch


def main() -> None:
    print(f"PyTorch: {torch.__version__}")
    print(f"CUDA available: {torch.cuda.is_available()}")
    if not torch.cuda.is_available():
        raise RuntimeError("CUDA is not available inside the task container")

    device = torch.device("cuda:0")
    properties = torch.cuda.get_device_properties(device)
    print(f"GPU: {properties.name}")
    print(f"VRAM: {properties.total_memory / 1024**3:.2f} GB")

    size = 2048
    left = torch.randn((size, size), device=device)
    right = torch.randn((size, size), device=device)
    torch.cuda.synchronize()
    started = time.perf_counter()
    result = left @ right
    torch.cuda.synchronize()

    print(f"Matrix: {size} x {size}")
    print(f"Elapsed: {time.perf_counter() - started:.3f} seconds")
    print(f"Checksum: {result.sum().item():.4f}")
    print("GPU_SMOKE_TEST_OK")


if __name__ == "__main__":
    main()

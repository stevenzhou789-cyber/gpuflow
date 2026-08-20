import json
import os
import time
from pathlib import Path

import torch
from torch import nn
from torch.utils.data import DataLoader, TensorDataset


def make_dataset(samples: int = 4096) -> TensorDataset:
    generator = torch.Generator().manual_seed(42)
    features = torch.randn(samples, 2, generator=generator)
    labels = (features[:, 0] + 0.7 * features[:, 1] > 0).long()
    return TensorDataset(features, labels)


def main() -> None:
    torch.manual_seed(42)
    device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    print(f"device={device}")
    if device.type == "cuda":
        print(f"gpu={torch.cuda.get_device_name(0)}")

    dataset = make_dataset()
    loader = DataLoader(dataset, batch_size=128, shuffle=True)
    model = nn.Sequential(
        nn.Linear(2, 16),
        nn.ReLU(),
        nn.Linear(16, 2),
    ).to(device)
    optimizer = torch.optim.Adam(model.parameters(), lr=0.01)
    loss_function = nn.CrossEntropyLoss()

    started = time.perf_counter()
    epochs = 20
    final_loss = 0.0
    for epoch in range(1, epochs + 1):
        model.train()
        total_loss = 0.0
        for features, labels in loader:
            features = features.to(device)
            labels = labels.to(device)
            optimizer.zero_grad()
            loss = loss_function(model(features), labels)
            loss.backward()
            optimizer.step()
            total_loss += loss.item()

        final_loss = total_loss / len(loader)
        print(f"epoch={epoch:02d} loss={final_loss:.6f}")

    model.eval()
    with torch.no_grad():
        features, labels = dataset.tensors
        predictions = model(features.to(device)).argmax(dim=1).cpu()
        accuracy = (predictions == labels).float().mean().item()

    elapsed = time.perf_counter() - started
    print(f"accuracy={accuracy:.4f}")
    print(f"elapsed_seconds={elapsed:.3f}")

    artifact_dir = Path(os.environ["GPUFLOW_ARTIFACT_DIR"])
    artifact_dir.mkdir(parents=True, exist_ok=True)
    torch.save(model.state_dict(), artifact_dir / "model.pt")
    metrics = {
        "device": str(device),
        "epochs": epochs,
        "final_loss": final_loss,
        "accuracy": accuracy,
        "elapsed_seconds": elapsed,
    }
    (artifact_dir / "metrics.json").write_text(
        json.dumps(metrics, indent=2, ensure_ascii=False),
        encoding="utf-8",
    )
    print(f"artifacts={artifact_dir}")
    print("TRAINING_OK")


if __name__ == "__main__":
    main()

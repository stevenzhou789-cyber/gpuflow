package api

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"gpuflow/internal/model"
	"gpuflow/internal/store"
)

type discoveryTestPublisher struct{ t *testing.T }

func (p discoveryTestPublisher) Publish(context.Context, io.Writer, string) (string, error) {
	p.t.Fatal("startup discovery must not publish images")
	return "", nil
}

func TestEnterpriseImageDiscoveryDoesNotReimportLocalBuildTags(t *testing.T) {
	storage := store.NewMemory()
	now := time.Now().UTC()
	published := model.TaskImage{
		ID: "img-published", Name: "registry.example.com/gpuflow-task/work:v1", Runtime: "shell",
		Status: "ready", CreatedAt: now, UpdatedAt: now,
	}
	if err := storage.SaveTaskImage(published); err != nil {
		t.Fatal(err)
	}
	listCalls := 0
	listLocalImages := func() ([]byte, error) {
		listCalls++
		return []byte("gpuflow-task/work:v1\ngpuflow-task/failed-push:v2\n"), nil
	}
	publisher := discoveryTestPublisher{t}
	builder := newImageBuilderWithDiscovery(storage, publisher, listLocalImages)
	if images := builder.List(); len(images) != 1 || images[0].ID != published.ID || images[0].Name != published.Name || images[0].Status != "ready" {
		t.Fatalf("restart changed the published image catalog: %+v", images)
	}
	// Removing the published reference leaves Docker's original local build
	// tag behind. It must not resurrect the deleted entry on another restart.
	builder.remove = func(_ context.Context, name string) ([]byte, error) {
		if name != published.Name {
			t.Fatalf("unexpected Docker removal: %s", name)
		}
		return nil, nil
	}
	if err := builder.Delete(published.ID); err != nil {
		t.Fatal(err)
	}
	restarted := newImageBuilderWithDiscovery(storage, publisher, listLocalImages)
	if images := restarted.List(); len(images) != 0 {
		t.Fatalf("deleted or unpublished local images reappeared: %+v", images)
	}
	if listCalls != 0 {
		t.Fatalf("enterprise startup queried the local Docker catalog %d times", listCalls)
	}
	if images, err := storage.ListTaskImages(); err != nil || len(images) != 0 {
		t.Fatalf("local images were persisted: images=%+v err=%v", images, err)
	}
}

func TestEnterpriseImageDiscoveryInvalidatesPreviouslyImportedLocalImages(t *testing.T) {
	storage := store.NewMemory()
	now := time.Now().UTC().Add(-time.Hour)
	legacy := model.TaskImage{
		ID: "img-legacy-local", Name: "gpuflow-task/work:v1", Runtime: "imported", BaseImage: "本机已有镜像",
		Status: "ready", CreatedAt: now, UpdatedAt: now,
	}
	if err := storage.SaveTaskImage(legacy); err != nil {
		t.Fatal(err)
	}
	qualified := legacy
	qualified.ID = "img-qualified"
	qualified.Name = "registry.example.com/gpuflow-task/work:v1"
	if err := storage.SaveTaskImage(qualified); err != nil {
		t.Fatal(err)
	}
	listLocalImages := func() ([]byte, error) {
		t.Fatal("enterprise startup must not query Docker")
		return nil, nil
	}
	for restart := 0; restart < 2; restart++ {
		builder := newImageBuilderWithDiscovery(storage, discoveryTestPublisher{t}, listLocalImages)
		image, err := builder.Get(legacy.ID)
		if err != nil || image.Status != "failed" || image.Error == "" || image.Name != legacy.Name || !image.CreatedAt.Equal(legacy.CreatedAt) {
			t.Fatalf("restart %d did not preserve and invalidate the legacy record: image=%+v err=%v", restart, image, err)
		}
		if ready, err := builder.Get(qualified.ID); err != nil || ready.Status != "ready" {
			t.Fatalf("registry-qualified image was invalidated: image=%+v err=%v", ready, err)
		}
		persisted, err := storage.ListTaskImages()
		if err != nil || len(persisted) != 2 {
			t.Fatalf("legacy repair was not persisted: images=%+v err=%v", persisted, err)
		}
		for _, saved := range persisted {
			if saved.ID == legacy.ID && (saved.Status != "failed" || saved.Error != image.Error) {
				t.Fatalf("legacy repair was not persisted: %+v", saved)
			}
		}
	}
}

type discoveryFailingSaveStore struct{ store.TaskImageStore }

func (discoveryFailingSaveStore) SaveTaskImage(model.TaskImage) error {
	return errors.New("persistence unavailable")
}

func TestEnterpriseImageDiscoveryFailsClosedWhenLegacyRepairCannotPersist(t *testing.T) {
	storage := store.NewMemory()
	legacy := model.TaskImage{ID: "img-legacy-local", Name: "gpuflow-task/work:v1", Runtime: "imported", Status: "ready"}
	if err := storage.SaveTaskImage(legacy); err != nil {
		t.Fatal(err)
	}
	builder := newImageBuilderWithDiscovery(discoveryFailingSaveStore{storage}, discoveryTestPublisher{t}, func() ([]byte, error) {
		t.Fatal("enterprise startup must not query Docker")
		return nil, nil
	})
	if image, err := builder.Get(legacy.ID); err != nil || image.Status != "failed" || image.Error == "" {
		t.Fatalf("unpublished image stayed ready after persistence failure: image=%+v err=%v", image, err)
	}
}

func TestCommunityImageDiscoveryPreservesLocalImportsAcrossRestarts(t *testing.T) {
	storage := store.NewMemory()
	listCalls := 0
	listLocalImages := func() ([]byte, error) {
		listCalls++
		return []byte("gpuflow-task/local:v1\ngpuflow-task/another:v2\npython:3.12\nregistry.example.com/gpuflow-task/remote:v1\n"), nil
	}
	first := newImageBuilderWithDiscovery(storage, nil, listLocalImages)
	firstImages := first.List()
	if len(firstImages) != 2 {
		t.Fatalf("community did not discover local task images: %+v", firstImages)
	}
	ids := make(map[string]string)
	for _, image := range firstImages {
		if image.Status != "ready" || image.Runtime != "imported" || !strings.HasPrefix(image.Name, "gpuflow-task/") {
			t.Fatalf("unexpected imported image: %+v", image)
		}
		ids[image.Name] = image.ID
	}
	restarted := newImageBuilderWithDiscovery(storage, nil, listLocalImages)
	if images := restarted.List(); len(images) != 2 {
		t.Fatalf("community restart duplicated imports: %+v", images)
	} else {
		for _, image := range images {
			if image.ID != ids[image.Name] || image.Status != "ready" {
				t.Fatalf("community restart changed a saved local image: %+v", image)
			}
		}
	}
	if listCalls != 2 {
		t.Fatalf("community startup queried Docker %d times, want 2", listCalls)
	}
}

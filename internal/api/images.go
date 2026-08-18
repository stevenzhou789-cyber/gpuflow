package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxTaskImageRequest = 6 << 20

var (
	imageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*(?::[a-zA-Z0-9][a-zA-Z0-9._-]*)?$`)
	filenamePattern  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	runtimeImages    = map[string]string{
		"shell":   "node:22-alpine",
		"python":  "python:3.12-slim",
		"cuda12":  "nvidia/cuda:12.0.1-base-ubuntu22.04",
		"pytorch": "pytorch/pytorch:2.1.2-cuda12.1-cudnn8-runtime",
	}
)

type TaskImage struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Runtime   string    `json:"runtime"`
	BaseImage string    `json:"base_image"`
	Filename  string    `json:"filename"`
	Command   string    `json:"command"`
	Status    string    `json:"status"`
	Log       string    `json:"log,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ImageBuilder struct {
	mu     sync.RWMutex
	images map[string]*TaskImage
	run    func(context.Context, string, ...string) ([]byte, error)
}

func NewImageBuilder() *ImageBuilder {
	return &ImageBuilder{
		images: make(map[string]*TaskImage),
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}
}

func (b *ImageBuilder) List() []TaskImage {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]TaskImage, 0, len(b.images))
	for _, image := range b.images {
		result = append(result, *image)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

func (s *Server) listTaskImages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.images.List())
}

func (s *Server) buildTaskImage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskImageRequest)
	if err := r.ParseMultipartForm(maxTaskImageRequest); err != nil {
		writeError(w, http.StatusBadRequest, "脚本与依赖总大小不能超过 6 MB")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	runtimeName := strings.TrimSpace(r.FormValue("runtime"))
	baseImage, ok := runtimeImages[runtimeName]
	if !ok {
		writeError(w, http.StatusBadRequest, "不支持的运行环境")
		return
	}
	imageName := strings.ToLower(strings.TrimSpace(r.FormValue("image")))
	if imageName == "" {
		imageName = "gpuflow-task/script:" + time.Now().Format("20060102-150405")
	}
	if len(imageName) > 180 || !imageNamePattern.MatchString(imageName) {
		writeError(w, http.StatusBadRequest, "镜像名称格式无效")
		return
	}

	file, header, err := r.FormFile("script")
	if err != nil {
		writeError(w, http.StatusBadRequest, "请选择任务脚本")
		return
	}
	defer file.Close()
	filename := filepath.Base(header.Filename)
	if len(filename) > 120 || !filenamePattern.MatchString(filename) {
		writeError(w, http.StatusBadRequest, "脚本文件名格式无效")
		return
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".py" && ext != ".sh" {
		writeError(w, http.StatusBadRequest, "仅支持 .py 或 .sh 脚本")
		return
	}
	if ext == ".py" && runtimeName == "cuda12" {
		writeError(w, http.StatusBadRequest, "CUDA 基础环境不含 Python，请选择 Python 或 PyTorch")
		return
	}

	script, err := io.ReadAll(io.LimitReader(file, 5<<20+1))
	if err != nil || len(script) == 0 || len(script) > 5<<20 {
		writeError(w, http.StatusBadRequest, "脚本必须为 1 byte 到 5 MB")
		return
	}
	requirements := strings.TrimSpace(r.FormValue("requirements"))
	if len(requirements) > 256<<10 {
		writeError(w, http.StatusBadRequest, "依赖内容不能超过 256 KB")
		return
	}
	if requirements != "" && runtimeName == "cuda12" {
		writeError(w, http.StatusBadRequest, "CUDA Shell 环境不支持 Python 依赖")
		return
	}

	id := randomID()
	now := time.Now().UTC()
	command := "sh /workspace/" + filename
	if ext == ".py" {
		command = "python /workspace/" + filename
	}
	image := &TaskImage{ID: id, Name: imageName, Runtime: runtimeName, BaseImage: baseImage, Filename: filename, Command: command, Status: "building", CreatedAt: now, UpdatedAt: now}
	s.images.mu.Lock()
	s.images.images[id] = image
	s.images.mu.Unlock()

	response := *image
	go s.images.build(id, script, requirements)
	writeJSON(w, http.StatusAccepted, response)
}

func (b *ImageBuilder) build(id string, script []byte, requirements string) {
	b.mu.RLock()
	image := *b.images[id]
	b.mu.RUnlock()

	dir, err := os.MkdirTemp("", "gpuflow-task-image-*")
	if err != nil {
		b.finish(id, nil, err)
		return
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, image.Filename), script, 0600); err != nil {
		b.finish(id, nil, err)
		return
	}
	dockerfile := taskDockerfile(image, requirements != "")
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
		b.finish(id, nil, err)
		return
	}
	if requirements != "" {
		if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte(requirements+"\n"), 0600); err != nil {
			b.finish(id, nil, err)
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	output, runErr := b.run(ctx, "docker", "build", "--tag", image.Name, dir)
	if ctx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("构建超过 15 分钟")
	}
	b.finish(id, output, runErr)
}

func taskDockerfile(image TaskImage, hasRequirements bool) string {
	var out bytes.Buffer
	fmt.Fprintf(&out, "FROM %s\nWORKDIR /workspace\n", image.BaseImage)
	if hasRequirements {
		out.WriteString("COPY requirements.txt /workspace/requirements.txt\nRUN python -m pip install --no-cache-dir -r /workspace/requirements.txt\n")
	}
	fmt.Fprintf(&out, "COPY %s /workspace/%s\n", image.Filename, image.Filename)
	command, _ := json.Marshal(strings.Fields(image.Command))
	fmt.Fprintf(&out, "CMD %s\n", command)
	return out.String()
}

func (b *ImageBuilder) finish(id string, output []byte, buildErr error) {
	if len(output) > 64<<10 {
		output = output[len(output)-(64<<10):]
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	image := b.images[id]
	image.Log = string(output)
	image.UpdatedAt = time.Now().UTC()
	if buildErr != nil {
		image.Status = "failed"
		image.Error = buildErr.Error()
		return
	}
	image.Status = "ready"
}

func randomID() string {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("img-%d", time.Now().UnixNano())
	}
	return "img-" + hex.EncodeToString(value[:])
}

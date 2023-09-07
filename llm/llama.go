package llm

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jmorganca/ollama/api"
)

//go:embed llama.cpp/*/build/*/bin/*
var llamaCppEmbed embed.FS

func osPath(llamaPath string) string {
	if runtime.GOOS == "windows" {
		return path.Join(llamaPath, "Release")
	}
	return llamaPath
}

func chooseRunner(gpuPath, cpuPath string) string {
	tmpDir, err := os.MkdirTemp("", "llama-*")
	if err != nil {
		log.Fatalf("llama.cpp: failed to create temp dir: %v", err)
	}

	llamaPath := osPath(gpuPath)
	if _, err := fs.Stat(llamaCppEmbed, llamaPath); err != nil {
		llamaPath = osPath(cpuPath)
		if _, err := fs.Stat(llamaCppEmbed, llamaPath); err != nil {
			log.Fatalf("llama.cpp executable not found")
		}
	}

	files := []string{"server"}
	switch runtime.GOOS {
	case "windows":
		files = []string{"server.exe"}
	case "darwin":
		if llamaPath == osPath(gpuPath) {
			files = append(files, "ggml-metal.metal")
		}
	case "linux":
		// check if there is a GPU available
		if _, err := CheckVRAM(); errors.Is(err, errNoGPU) {
			// this error was logged on start-up, so we don't need to log it again
			llamaPath = osPath(cpuPath)
		}
	}

	for _, f := range files {
		srcPath := path.Join(llamaPath, f)
		destPath := filepath.Join(tmpDir, f)

		srcFile, err := llamaCppEmbed.Open(srcPath)
		if err != nil {
			log.Fatalf("read llama.cpp %s: %v", f, err)
		}
		defer srcFile.Close()

		destFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			log.Fatalf("write llama.cpp %s: %v", f, err)
		}
		defer destFile.Close()

		if _, err := io.Copy(destFile, srcFile); err != nil {
			log.Fatalf("copy llama.cpp %s: %v", f, err)
		}
	}

	runPath := filepath.Join(tmpDir, "server")
	if runtime.GOOS == "windows" {
		runPath = filepath.Join(tmpDir, "server.exe")
	}

	return runPath
}

const ModelFamilyLlama ModelFamily = "llama"

type llamaModel struct {
	hyperparameters llamaHyperparameters
}

func (llm *llamaModel) ModelFamily() ModelFamily {
	return ModelFamilyLlama
}

func (llm *llamaModel) ModelType() ModelType {
	switch llm.hyperparameters.NumLayer {
	case 26:
		return ModelType3B
	case 32:
		return ModelType7B
	case 40:
		return ModelType13B
	case 48:
		return ModelType34B
	case 60:
		return ModelType30B
	case 80:
		return ModelType65B
	}

	// TODO: find a better default
	return ModelType7B
}

func (llm *llamaModel) FileType() FileType {
	return llm.hyperparameters.FileType
}

type llamaHyperparameters struct {
	// NumVocab is the size of the model's vocabulary.
	NumVocab uint32

	// NumEmbd is the size of the model's embedding layer.
	NumEmbd uint32
	NumMult uint32
	NumHead uint32

	// NumLayer is the number of layers in the model.
	NumLayer uint32
	NumRot   uint32

	// FileType describes the quantization level of the model, e.g. Q4_0, Q5_K, etc.
	FileType llamaFileType
}

type llamaFileType uint32

const (
	llamaFileTypeF32 llamaFileType = iota
	llamaFileTypeF16
	llamaFileTypeQ4_0
	llamaFileTypeQ4_1
	llamaFileTypeQ4_1_F16
	llamaFileTypeQ8_0 llamaFileType = iota + 2
	llamaFileTypeQ5_0
	llamaFileTypeQ5_1
	llamaFileTypeQ2_K
	llamaFileTypeQ3_K_S
	llamaFileTypeQ3_K_M
	llamaFileTypeQ3_K_L
	llamaFileTypeQ4_K_S
	llamaFileTypeQ4_K_M
	llamaFileTypeQ5_K_S
	llamaFileTypeQ5_K_M
	llamaFileTypeQ6_K
)

func (ft llamaFileType) String() string {
	switch ft {
	case llamaFileTypeF32:
		return "F32"
	case llamaFileTypeF16:
		return "F16"
	case llamaFileTypeQ4_0:
		return "Q4_0"
	case llamaFileTypeQ4_1:
		return "Q4_1"
	case llamaFileTypeQ4_1_F16:
		return "Q4_1_F16"
	case llamaFileTypeQ8_0:
		return "Q8_0"
	case llamaFileTypeQ5_0:
		return "Q5_0"
	case llamaFileTypeQ5_1:
		return "Q5_1"
	case llamaFileTypeQ2_K:
		return "Q2_K"
	case llamaFileTypeQ3_K_S:
		return "Q3_K_S"
	case llamaFileTypeQ3_K_M:
		return "Q3_K_M"
	case llamaFileTypeQ3_K_L:
		return "Q3_K_L"
	case llamaFileTypeQ4_K_S:
		return "Q4_K_S"
	case llamaFileTypeQ4_K_M:
		return "Q4_K_M"
	case llamaFileTypeQ5_K_S:
		return "Q5_K_S"
	case llamaFileTypeQ5_K_M:
		return "Q5_K_M"
	case llamaFileTypeQ6_K:
		return "Q6_K"
	default:
		return "Unknown"
	}
}

type Running struct {
	Port   int
	Cmd    *exec.Cmd
	Cancel context.CancelFunc
}

type ModelRunner struct {
	Path string // path to the model runner executable
}

type llama struct {
	api.Options
	Running
}

var errNoGPU = errors.New("nvidia-smi command failed")

// CheckVRAM returns the available VRAM in MiB on Linux machines with NVIDIA GPUs
func CheckVRAM() (int, error) {
	return 23000, nil
	// cmd := exec.Command("nvidia-smi", "--query-gpu=memory.free", "--format=csv,noheader,nounits")
	// var stdout bytes.Buffer
	// cmd.Stdout = &stdout
	// err := cmd.Run()
	// if err != nil {
	// 	return 0, errNoGPU
	// }

	// var total int
	// scanner := bufio.NewScanner(&stdout)
	// for scanner.Scan() {
	// 	line := scanner.Text()
	// 	vram, err := strconv.Atoi(line)
	// 	if err != nil {
	// 		return 0, fmt.Errorf("failed to parse available VRAM: %v", err)
	// 	}

	// 	total += vram
	// }

	// return total, nil
}

func NumGPU(opts api.Options) int {
	return 48

	// if opts.NumGPU != -1 {
	// 	return opts.NumGPU
	// }
	// n := 1 // default to enable metal on macOS
	// if runtime.GOOS == "linux" {
	// 	vram, err := CheckVRAM()
	// 	if err != nil {
	// 		if err.Error() != "nvidia-smi command failed" {
	// 			log.Print(err.Error())
	// 		}
	// 		// nvidia driver not installed or no nvidia GPU found
	// 		return 0
	// 	}
	// 	// TODO: this is a very rough heuristic, better would be to calculate this based on number of layers and context size
	// 	switch {
	// 	case vram < 500:
	// 		log.Printf("WARNING: Low VRAM detected, disabling GPU")
	// 		n = 0
	// 	case vram < 1000:
	// 		n = 4
	// 	case vram < 2000:
	// 		n = 8
	// 	case vram < 4000:
	// 		n = 12
	// 	case vram < 8000:
	// 		n = 16
	// 	case vram < 12000:
	// 		n = 24
	// 	case vram < 16000:
	// 		n = 32
	// 	default:
	// 		n = 48
	// 	}
	// 	log.Printf("%d MB VRAM available, loading %d GPU layers", vram, n)
	// }
	// return n
}

func newLlama(model string, adapters []string, runner ModelRunner, opts api.Options) (*llama, error) {
	if _, err := os.Stat(model); err != nil {
		return nil, err
	}

	if _, err := os.Stat(runner.Path); err != nil {
		return nil, err
	}

	if len(adapters) > 1 {
		return nil, errors.New("ollama supports only one lora adapter, but multiple were provided")
	}

	params := []string{
		"--model", model,
		"--ctx-size", fmt.Sprintf("%d", opts.NumCtx),
		"--rope-freq-base", fmt.Sprintf("%f", opts.RopeFrequencyBase),
		"--rope-freq-scale", fmt.Sprintf("%f", opts.RopeFrequencyScale),
		"--batch-size", fmt.Sprintf("%d", opts.NumBatch),
		"--n-gpu-layers", fmt.Sprintf("%d", NumGPU(opts)),
		"--embedding",
	}

	if opts.NumGQA > 0 {
		params = append(params, "--gqa", fmt.Sprintf("%d", opts.NumGQA))
	}

	if len(adapters) > 0 {
		// TODO: applying multiple adapters is not supported by the llama.cpp server yet
		params = append(params, "--lora", adapters[0])
	}

	if opts.NumThread > 0 {
		params = append(params, "--threads", fmt.Sprintf("%d", opts.NumThread))
	}

	if !opts.F16KV {
		params = append(params, "--memory-f32")
	}
	if opts.UseMLock {
		params = append(params, "--mlock")
	}
	if !opts.UseMMap {
		params = append(params, "--no-mmap")
	}
	if opts.UseNUMA {
		params = append(params, "--numa")
	}

	// start the llama.cpp server with a retry in case the port is already in use
	for try := 0; try < 3; try++ {
		port := rand.Intn(65535-49152) + 49152 // get a random port in the ephemeral range
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(
			ctx,
			runner.Path,
			append(params, "--port", strconv.Itoa(port))...,
		)

		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr

		llm := &llama{Options: opts, Running: Running{Port: port, Cmd: cmd, Cancel: cancel}}

		log.Print("starting llama.cpp server")
		if err := llm.Cmd.Start(); err != nil {
			log.Printf("error starting the external llama.cpp server: %v", err)
			continue
		}

		if err := waitForServer(llm); err != nil {
			log.Printf("error starting llama.cpp server: %v", err)
			llm.Close()
			// try again
			continue
		}

		// server started successfully
		return llm, nil
	}

	return nil, fmt.Errorf("max retry exceeded starting llama.cpp")
}

func waitForServer(llm *llama) error {
	// wait for the server to start responding
	start := time.Now()
	expiresAt := time.Now().Add(45 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)

	log.Print("waiting for llama.cpp server to start responding")
	for range ticker.C {
		if time.Now().After(expiresAt) {
			return fmt.Errorf("llama.cpp server did not start within alloted time, retrying")
		}

		if err := llm.Ping(context.Background()); err == nil {
			break
		}
	}

	log.Printf("llama.cpp server started in %f seconds", time.Since(start).Seconds())
	return nil
}

func (llm *llama) Close() {
	llm.Cancel()
	if err := llm.Cmd.Wait(); err != nil {
		log.Printf("llama.cpp server exited with error: %v", err)
	}
}

func (llm *llama) SetOptions(opts api.Options) {
	llm.Options = opts
}

type GenerationSettings struct {
	FrequencyPenalty float64       `json:"frequency_penalty"`
	IgnoreEOS        bool          `json:"ignore_eos"`
	LogitBias        []interface{} `json:"logit_bias"`
	Mirostat         int           `json:"mirostat"`
	MirostatEta      float64       `json:"mirostat_eta"`
	MirostatTau      float64       `json:"mirostat_tau"`
	Model            string        `json:"model"`
	NCtx             int           `json:"n_ctx"`
	NKeep            int           `json:"n_keep"`
	NPredict         int           `json:"n_predict"`
	NProbs           int           `json:"n_probs"`
	PenalizeNl       bool          `json:"penalize_nl"`
	PresencePenalty  float64       `json:"presence_penalty"`
	RepeatLastN      int           `json:"repeat_last_n"`
	RepeatPenalty    float64       `json:"repeat_penalty"`
	Seed             uint32        `json:"seed"`
	Stop             []string      `json:"stop"`
	Stream           bool          `json:"stream"`
	Temp             float64       `json:"temp"`
	TfsZ             float64       `json:"tfs_z"`
	TopK             int           `json:"top_k"`
	TopP             float64       `json:"top_p"`
	TypicalP         float64       `json:"typical_p"`
}

type Timings struct {
	PredictedN  int     `json:"predicted_n"`
	PredictedMS float64 `json:"predicted_ms"`
	PromptN     int     `json:"prompt_n"`
	PromptMS    float64 `json:"prompt_ms"`
}

type Prediction struct {
	Content string `json:"content"`
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	Stop    bool   `json:"stop"`

	Timings `json:"timings"`
}

type PredictRequest struct {
	Stream           bool            `json:"stream"`
	NPredict         int             `json:"n_predict,omitempty"`
	TopK             int             `json:"top_k,omitempty"`
	TopP             float32         `json:"top_p,omitempty"`
	TfsZ             float32         `json:"tfs_z,omitempty"`
	TypicalP         float32         `json:"typical_p,omitempty"`
	RepeatLastN      int             `json:"repeat_last_n,omitempty"`
	Temperature      float32         `json:"temperature,omitempty"`
	RepeatPenalty    float32         `json:"repeat_penalty,omitempty"`
	PresencePenalty  float32         `json:"presence_penalty,omitempty"`
	FrequencyPenalty float32         `json:"frequency_penalty,omitempty"`
	Mirostat         int             `json:"mirostat,omitempty"`
	MirostatTau      float32         `json:"mirostat_tau,omitempty"`
	MirostatEta      float32         `json:"mirostat_eta,omitempty"`
	PenalizeNl       bool            `json:"penalize_nl,omitempty"`
	NKeep            int             `json:"n_keep,omitempty"`
	Seed             int             `json:"seed,omitempty"`
	Prompt           string          `json:"prompt,omitempty"`
	NProbs           int             `json:"n_probs,omitempty"`
	LogitBias        map[int]float32 `json:"logit_bias,omitempty"`
	IgnoreEos        bool            `json:"ignore_eos,omitempty"`
	Stop             []string        `json:"stop,omitempty"`
}

func (llm *llama) Predict(ctx context.Context, prevContext []int, prompt string, fn func(api.GenerateResponse)) error {
	prevConvo, err := llm.Decode(ctx, prevContext)
	if err != nil {
		return err
	}

	var nextContext strings.Builder
	nextContext.WriteString(prevConvo)
	nextContext.WriteString(prompt)

	endpoint := fmt.Sprintf("http://127.0.0.1:%d/completion", llm.Port)
	predReq := PredictRequest{
		Prompt:           nextContext.String(),
		Stream:           true,
		NPredict:         llm.NumPredict,
		NKeep:            llm.NumKeep,
		Temperature:      llm.Temperature,
		TopK:             llm.TopK,
		TopP:             llm.TopP,
		TfsZ:             llm.TFSZ,
		TypicalP:         llm.TypicalP,
		RepeatLastN:      llm.RepeatLastN,
		RepeatPenalty:    llm.RepeatPenalty,
		PresencePenalty:  llm.PresencePenalty,
		FrequencyPenalty: llm.FrequencyPenalty,
		Mirostat:         llm.Mirostat,
		MirostatTau:      llm.MirostatTau,
		MirostatEta:      llm.MirostatEta,
		PenalizeNl:       llm.PenalizeNewline,
		Stop:             llm.Stop,
	}
	data, err := json.Marshal(predReq)
	if err != nil {
		return fmt.Errorf("error marshaling data: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(data))
	if err != nil {
		return fmt.Errorf("error creating POST request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST predict: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed reading llm error response: %w", err)
		}
		log.Printf("llm predict error: %s", bodyBytes)
		return fmt.Errorf("%s", bodyBytes)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			// This handles the request cancellation
			return ctx.Err()
		default:
			line := scanner.Text()
			if line == "" {
				continue
			}

			// Read data from the server-side event stream
			if strings.HasPrefix(line, "data: ") {
				evt := line[6:]
				var p Prediction
				if err := json.Unmarshal([]byte(evt), &p); err != nil {
					return fmt.Errorf("error unmarshaling llm prediction response: %v", err)
				}

				fn(api.GenerateResponse{Response: p.Content})
				nextContext.WriteString(p.Content)

				if p.Stop {
					embd, err := llm.Encode(ctx, nextContext.String())
					if err != nil {
						return fmt.Errorf("encoding context: %v", err)
					}

					fn(api.GenerateResponse{
						Done:               true,
						Context:            embd,
						PromptEvalCount:    p.PromptN,
						PromptEvalDuration: parseDurationMs(p.PromptMS),
						EvalCount:          p.PredictedN,
						EvalDuration:       parseDurationMs(p.PredictedMS),
					})

					return nil
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading llm response: %v", err)
	}

	return nil
}

type TokenizeRequest struct {
	Content string `json:"content"`
}

type TokenizeResponse struct {
	Tokens []int `json:"tokens"`
}

func (llm *llama) Encode(ctx context.Context, prompt string) ([]int, error) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/tokenize", llm.Port)
	data, err := json.Marshal(TokenizeRequest{Content: prompt})
	if err != nil {
		return nil, fmt.Errorf("marshaling encode data: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do encode request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read encode request: %w", err)
	}

	if resp.StatusCode >= 400 {
		log.Printf("llm encode error: %s", body)
		return nil, fmt.Errorf("%s", body)
	}

	var encoded TokenizeResponse
	if err := json.Unmarshal(body, &encoded); err != nil {
		return nil, fmt.Errorf("unmarshal encode response: %w", err)
	}

	return encoded.Tokens, nil
}

type DetokenizeRequest struct {
	Tokens []int `json:"tokens"`
}

type DetokenizeResponse struct {
	Content string `json:"content"`
}

func (llm *llama) Decode(ctx context.Context, tokens []int) (string, error) {
	if len(tokens) == 0 {
		return "", nil
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/detokenize", llm.Port)
	data, err := json.Marshal(DetokenizeRequest{Tokens: tokens})
	if err != nil {
		return "", fmt.Errorf("marshaling decode data: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(data))
	if err != nil {
		return "", fmt.Errorf("decode request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("do decode request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read decode request: %w", err)
	}

	if resp.StatusCode >= 400 {
		log.Printf("llm decode error: %s", body)
		return "", fmt.Errorf("%s", body)
	}

	var decoded DetokenizeResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("unmarshal encode response: %w", err)
	}

	// decoded content contains a leading whitespace
	decoded.Content, _ = strings.CutPrefix(decoded.Content, "")

	return decoded.Content, nil
}

type EmbeddingRequest struct {
	Content string `json:"content"`
}

type EmbeddingResponse struct {
	Embedding []float64 `json:"embedding"`
}

func (llm *llama) Embedding(ctx context.Context, input string) ([]float64, error) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/embedding", llm.Port)
	data, err := json.Marshal(TokenizeRequest{Content: input})
	if err != nil {
		return nil, fmt.Errorf("error marshaling embed data: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("error creating embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST embedding: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading embed response: %w", err)
	}

	if resp.StatusCode >= 400 {
		log.Printf("llm encode error: %s", body)
		return nil, fmt.Errorf("%s", body)
	}

	var embedding EmbeddingResponse
	if err := json.Unmarshal(body, &embedding); err != nil {
		return nil, fmt.Errorf("unmarshal tokenize response: %w", err)
	}

	return embedding.Embedding, nil
}

// Ping checks that the server subprocess is still running and responding to requests
func (llm *llama) Ping(ctx context.Context) error {
	resp, err := http.Head(fmt.Sprintf("http://127.0.0.1:%d", llm.Port))
	if err != nil {
		return fmt.Errorf("ping resp: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected ping status: %s", resp.Status)
	}
	return nil
}

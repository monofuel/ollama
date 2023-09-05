//go:build linux && gpu
// +build linux,gpu

package llm

//go:generate git submodule init
//go:generate git submodule update --force ggml gguf
//go:generate git -C ggml apply ../ggml_patch/0001-add-detokenize-endpoint.patch
//go:generate git -C ggml apply ../ggml_patch/0002-34B-model-support.patch
//go:generate cmake --force -S ggml -B ggml/build/gpu -DLLAMA_CUBLAS=on -DLLAMA_ACCELERATE=on -DLLAMA_K_QUANTS=on
//go:generate cmake --build ggml/build/gpu --target server --config Release
//go:generate cmake --force -S gguf -B gguf/build/gpu -DLLAMA_CUBLAS=on -DLLAMA_ACCELERATE=on -DLLAMA_K_QUANTS=on
//go:generate cmake --build gguf/build/gpu --target server --config Release

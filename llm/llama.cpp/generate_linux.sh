#!/bin/bash

cmake --fresh -S ggml -B ggml/build/cpu -DLLAMA_CLBLAST=on -DLLAMA_ACCELERATE=on -DLLAMA_K_QUANTS=on
cmake --build ggml/build/cpu --target server --config Release

cmake --fresh -S gguf -B gguf/build/gpu -DLLAMA_CLBLAST=on -DLLAMA_ACCELERATE=on -DLLAMA_K_QUANTS=on
cmake --build gguf/build/gpu --target server --config Release

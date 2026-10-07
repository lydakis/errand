#!/bin/sh
set -eu
export PYTHONUNBUFFERED=1
if ! python3 -c 'import torch; assert torch.cuda.is_available()' 2>/dev/null; then
    echo 'CUDA-enabled PyTorch is missing from this image; stopping to avoid a large install.'
    exit 1
fi
exec python3 -u train.py

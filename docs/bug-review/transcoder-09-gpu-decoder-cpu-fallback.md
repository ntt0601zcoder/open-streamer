# Fallback decoder sang CPU trên đường GPU đưa frame phần mềm vào scale_cuda

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder/native
- **Vị trí:** internal/transcoder/native/decoder.go:114-132, internal/transcoder/native/stream_pipeline.go:368, internal/transcoder/native/stream_pipeline.go:748-756, internal/transcoder/native/stream_pipeline.go:1126-1129, internal/transcoder/native/cuda.go:219-223, internal/transcoder/native/stream_pipeline.go:1408-1409

## Hiện tượng
Khi NVDEC không mở được cho codec nguồn, ví dụ `hevc_cuvid` trên GPU không hỗ trợ HEVC, log báo "falling back to CPU" nhưng ngay sau đó subprocess lỗi ở `gpu scaler` và respawn lặp lại.

## Điều kiện kích hoạt
CUDA device tạo được (nên `useGPU=true`), nhưng `NewDecoder` với `*_cuvid` thất bại lúc khởi tạo pipeline, lúc `ensureVideoDecoder` đổi codec, hoặc lúc `switchInputGPU`.

## Phân tích
`newDecoderWithFallback` tự hạ về CPU và trả decoder ra RAM, nhưng người gọi không biết:

```go
cfg.Codec = cpu
return NewDecoder(cfg)
```

`p.useGPU` vẫn là true, nên frame YUV đi vào `gpuScalers[i].Scale(f)`. Graph luôn khai báo pixel format CUDA và chỉ gắn frames context khi có:

```go
params.SetPixelFormat(astiav.PixelFormatCuda)
if hfc := in.HardwareFramesContext(); hfc != nil { ... }
```

Frame phần mềm không có `hw_frames_ctx`, nên buffersrc không thể cấu hình cho định dạng phần cứng. Lỗi trở thành terminal error.

## Vì sao đây là lỗi
Comment của `newDecoderWithFallback` nói mục đích là "keeps the stream alive on CPU instead of ... respawn-looping". Điều đó chỉ đúng khi `useGPU=false`, mà khi đó decoder đã được hạ về CPU từ trước và không cần fallback. Scaler CPU và encoder input CPU không được dựng cho đường GPU, nên không thể chuyển nóng.

## Hướng sửa
Với đường GPU, bỏ fallback âm thầm và trả lỗi rõ ràng, hoặc hỗ trợ chèn `hwupload_cuda` khi frame là phần mềm. Khi khởi tạo pipeline, có thể mở decoder trước khi chọn đường GPU, rồi hạ cả pipeline về CPU nếu cuvid không dùng được.

## Cách xác minh
Trên GPU không có HEVC NVDEC, đưa nguồn HEVC vào stream `hw=nvenc`. Hoặc test bằng cách đưa frame `YUV420P` vào `gpuScaler.Scale` và kiểm tra lỗi ở `ensureGraph`.

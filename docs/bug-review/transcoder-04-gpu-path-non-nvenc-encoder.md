# Backend NVENC với encoder không phải NVENC vẫn đi đường GPU và nhận frame CUDA

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder/native, internal/domain
- **Vị trí:** internal/transcoder/native/stream_pipeline.go:292-305, internal/transcoder/native/stream_pipeline.go:343-355, internal/transcoder/native/stream_pipeline.go:1408-1434, internal/transcoder/native/encoder.go:114-119, internal/transcoder/native/server.go:425-430, internal/domain/resolvers.go:23-42, internal/domain/resolvers.go:60-67

## Hiện tượng
Stream có `global.hw=nvenc` nhưng một profile dùng `codec: av1`, `mp2v` hoặc tên encoder CPU như `libx264` không bao giờ ra output của profile đó. Subprocess lỗi hoặc crash và respawn lặp lại, làm hỏng cả ladder.

## Điều kiện kích hoạt
`hw=nvenc`, CUDA khởi tạo được, và ít nhất một profile resolve ra encoder không có hậu tố `_nvenc`.

## Phân tích
Decoder được chọn chỉ theo backend (`h264_cuvid` khi NVENC). Đường GPU được quyết định chỉ theo decoder:

```go
useGPU := isCUVIDDecoder(cfg.Decoder.Codec)
```

`ResolveVideoEncoder` trả `libsvtav1` cho `av1`, `mpeg2video` cho `mp2v`, và giữ nguyên `libx264`. Trong `NewEncoder`, nhánh GPU cần cả tên NVENC:

```go
gpuInput := cfg.cuda != nil && cfg.cuda.dev != nil && isNVENCEncoder(codecName)
```

Nên encoder mở với `YUV420P`, trong khi `encodeOneRendition` vẫn đưa cho nó frame từ `gpuScalers[i].Scale(f)`, tức frame `AV_PIX_FMT_CUDA` mà `data[]` là handle GPU chứ không phải plane RAM.

## Vì sao đây là lỗi
Comment của struct nói đường GPU "chỉ chọn cho NVENC + cuvid", nhưng code không kiểm tra encoder của từng rendition. Không có `hwdownload` hay chuyển định dạng giữa `gpuScaler` và encoder CPU. Cách fail cụ thể (lỗi từ libav hay SIGSEGV) chưa chạy thử để xác nhận, nhưng frame đó không thể encode hợp lệ.

## Hướng sửa
Chỉ bật `useGPU` khi mọi rendition dùng encoder NVENC, nếu không thì hạ decoder về CPU như nhánh fallback hiện có. Hoặc từ chối tổ hợp này khi validate cấu hình.

## Cách xác minh
Trên máy có NVIDIA, cấu hình `hw=nvenc` với một profile `codec: libx264` và quan sát log subprocess. Có thể thêm test đơn vị cho hàm quyết định `useGPU` sau khi tách riêng.

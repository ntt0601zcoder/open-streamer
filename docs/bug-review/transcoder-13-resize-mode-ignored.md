# resize_mode, sar và refs bị bỏ qua nên nguồn khác tỉ lệ luôn bị kéo méo

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder, internal/transcoder/native, internal/domain
- **Vị trí:** internal/transcoder/supervisor.go:560-562, internal/transcoder/native/server.go:283-328, internal/transcoder/native/scaler.go:87, internal/transcoder/native/cuda.go:134, internal/domain/transcoder.go:115-121, internal/domain/resolvers.go:97-108, docs/CONFIG.md:311, docs/ARCHITECTURE.md:461-463

## Hiện tượng
Nguồn 4:3 vào rendition 16:9 luôn bị kéo giãn hình, dù mặc định tài liệu là `pad` (letterbox). Đổi `resize_mode` sang `fit`, `crop` hay `pad` không có tác dụng. `sar` và `refs` cũng không có hiệu lực.

## Điều kiện kích hoạt
Tỉ lệ khung hình nguồn khác tỉ lệ `width:height` của profile, với bất kỳ `resize_mode` nào.

## Phân tích
Supervisor gửi `Sar`, `ResizeMode`, `RefsOrNeg1` qua proto, nhưng `pipelineConfigFromProto` không đọc các field đó. `ScalerConfig` chỉ có `DstWidth`, `DstHeight`, `DstPixelFmt`. Cả hai đường scale đều ép đúng kích thước đích:

```go
s.sws.ScaleFrame(src, dst)                        // CPU
scale := fmt.Sprintf("scale_cuda=%d:%d:format=nv12", dstW, dstH) // GPU
```

## Vì sao đây là lỗi
`domain.ResizeMode` quy định `pad` là mặc định, API trả mặc định đó, và `docs/ARCHITECTURE.md` còn nói pad/crop trên GPU rơi về fit giữ tỉ lệ. Thực tế cả CPU và GPU đều là stretch.

## Hướng sửa
Đọc `ResizeMode` trong `pipelineConfigFromProto` và tính kích thước, vùng crop/pad theo tỉ lệ nguồn cho cả hai đường. GPU có thể dùng scale giữ tỉ lệ rồi nối pad bằng filter tương ứng. Áp dụng `SAR` và `Refs` lên encoder, hoặc gỡ các field không hỗ trợ khỏi API.

## Cách xác minh
Transcode nguồn 640x480 sang 1280x720 với `resize_mode: pad`, xem frame output có viền đen hay bị kéo. Test đơn vị cho `pipelineConfigFromProto` để khẳng định các field được giữ lại.

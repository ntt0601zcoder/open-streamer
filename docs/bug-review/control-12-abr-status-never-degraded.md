# Stream ABR copy và ABR mixer luôn báo Active dù upstream đã chết
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** coordinator, api/handler, metrics
- **Vị trí:** internal/coordinator/coordinator.go:223-234, 240-253; internal/coordinator/abr_copy.go:338-355; internal/coordinator/abr_mixer.go:517-536; internal/api/handler/stream.go:107, 115-122; internal/coordinator/coordinator.go:1002

## Hiện tượng
Upstream dừng hoặc không có dữ liệu. Stream downstream không có hình nhưng `status` vẫn `active`, trong khi `runtime.inputs[0].status` là `degraded`. Gauge `open_streamer_streams_total` và bộ lọc `GET /streams?status=degraded` không tính stream này. Không có event degraded.

## Điều kiện kích hoạt
Stream ABR copy/mixer đang chạy, upstream bị disable, bị xoá hoặc mất nguồn trên 30 giây.

## Phân tích
`StreamStatus` chỉ đọc map `degradation`, và map này chỉ được đặt qua callback của manager và transcoder. Stream ABR không đi qua hai thành phần đó. Hàm `ABRCopyRuntimeStatus` có tính được health từ `lastPacketAtNanos`, nhưng kết quả chỉ nằm ở input giả:
```go
Status:              domain.StatusActive, // pipeline-level (filled by handler)
```
Handler sau đó ghi đè bằng `StreamStatus`, tức vẫn là `Active`.

## Vì sao đây là lỗi
Bảng invariant trong ARCHITECTURE.md ghi "Stream status reflects all degradation sources" với lý do "UI green badge must mean actually working". FEATURES_CHECKLIST chỉ ghi nhận `runtime.media` trống trên các stream này, không nói tới status.

## Hướng sửa
Trong `StreamStatus`, với entry ABR, trả `Degraded` khi `lastPacketAtNanos` quá `abrCopyInputStaleAfter`. Nếu cần event, thêm chuyển trạng thái trong tap hoặc reconciler.

## Cách xác minh
Test: tạo entry ABR với `lastPacketAtNanos` cũ hơn 30 giây, kỳ vọng `StreamStatus` trả `degraded`. Hiện tại trả `active`.

# Tắt sessions lúc đang chạy làm các session HLS/DASH và gauge đứng yên mãi

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/sessions, internal/runtime
- **Vị trí:** internal/sessions/reaper.go:17-20, internal/sessions/reaper.go:29-32, internal/sessions/tracker.go:291-294

## Hiện tượng
Sau khi đổi `sessions.enabled=false` bằng `/config`, `/sessions` vẫn liệt kê các viewer cũ vô thời hạn và `open_streamer_sessions_active` giữ nguyên giá trị.

## Điều kiện kích hoạt
Có session HLS/DASH đang hoạt động, rồi hot-reload `sessions.enabled` từ true sang false.

## Phân tích
```go
case <-time.After(tick):
    if cfg.enabled {
        s.reapOnce(ctx)
    }
```
`TrackHTTP` trả `nil` ngay khi disabled nên `UpdatedAt` không đổi nữa, còn HTTP session chỉ được đóng bởi reaper. Map giữ nguyên trạng thái đó. Connection-bound vẫn được đóng qua `Closer`.

## Vì sao đây là lỗi
Comment ghi "reapOnce is a no-op (no sessions to scan)", nhưng sessions vẫn còn trong map vì `UpdateConfig` cố ý giữ map. `List`, `Stats` không kiểm tra `enabled`.

## Hướng sửa
Luôn chạy `reapOnce` bất kể `enabled`; hoặc khi chuyển sang disabled thì đóng hết session với lý do phù hợp.

## Cách xác minh
Unit test: `TrackHTTP`, `UpdateConfig(enabled=false)`, tiến đồng hồ, chạy một vòng `runReaper`, kiểm tra `Stats().Active` vẫn là 1.

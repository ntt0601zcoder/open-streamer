# Rớt kết nối RTMP, RTSP, SRT bị coi là EOF nên worker dừng thay vì reconnect
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor, ingestor/pull, manager
- **Vị trí:** internal/ingestor/worker.go:284-296; internal/ingestor/pull/rtmp.go:146-148, 157-172, 200-203; internal/ingestor/pull/rtsp.go:446-463, 485-488; internal/ingestor/pull/srt.go:125-129; internal/manager/service.go:48, 870-889

## Hiện tượng
Một sự cố mạng ngắn trên input RTMP/RTSP/SRT đẩy stream một input vào Exhausted, phát hook lỗi và mất hình khoảng 10 s trở lên, thay vì reconnect sau 1 s. Stream có dự phòng thì chuyển sang dự phòng rồi quay lại sau ít nhất 12 s, thành hai lần chuyển mạch.

## Điều kiện kích hoạt
Input pull RTMP, RTSP hoặc SRT mất kết nối: reset TCP, server restart, SRT peer idle timeout.

## Phân tích
Reader chuyển mọi kiểu kết thúc thành EOF. Lý do lỗi chỉ được log rồi bị bỏ:
```go
// rtmp.go
if err := <-session.WaitChan(); err != nil { slog.Debug(...) }
...
close(pkts)
```
`rtsp.go` làm tương tự trong `waitLoop`; `srt.go:125` map `ErrClientClosed` sang `io.EOF`. Worker coi EOF là nguồn đã hết:
```go
if errors.Is(readErr, io.EOF) {
    ...
    cb.onInputError(streamID, input.Priority, io.EOF)
    return true
}
```
Đường phục hồi khi không có dự phòng: Degraded → Exhausted → đợi `failbackProbeCooldown` 8 s → Probe ít nhất 1,5 s → `Start`.

## Vì sao đây là lỗi
Comment ở `rtmp.go:146-148` viết rõ "ReadPackets then returns io.EOF so the worker reconnects via the standard backoff", và `ARCHITECTURE.md` nói "Reconnect is automatic". Thực tế nhánh backoff không bao giờ chạy cho các reader này. Với file không lặp hoặc HLS `ENDLIST`, EOF đúng nghĩa; với kết nối live thì không.

## Hướng sửa
Phân biệt kết thúc thật với lỗi: lưu lý do từ `WaitChan`/`c.Wait` và trả lỗi không phải EOF (như `TSDemuxPacketReader.readErr`). Coi kết thúc do server với nguồn live là retry được, có giới hạn số lần trước khi báo manager.

## Cách xác minh
Unit test `handleReadError` kèm reader RTMP giả đóng session: hiện worker trả về thay vì reconnect. Thủ công trên môi trường thử: pull từ một server RTMP thử nghiệm, restart server đó, so thời gian tới packet đầu tiên và các event `input.failed`.

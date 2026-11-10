# Marker SessionStart một lần bị mất với subscriber có channel đầy
- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** buffer, publisher, transcoder, dvr
- **Vị trí:** internal/buffer/service.go:63-79; internal/publisher/hls.go:311; internal/publisher/dash/packager.go:298; internal/transcoder/supervisor.go:316; internal/dvr/blob/writer.go:103

## Hiện tượng
Sau failover hoặc reconnect, một consumer đang chậm không reset trạng thái: HLS không có `EXT-X-DISCONTINUITY`, DASH giữ queue cũ, transcoder không gọi `SwitchInput` nên decoder cũ nhận tham số SPS/PPS mới, DVR không đánh dấu discontinuity. Các consumer khác vẫn bình thường.

## Điều kiện kích hoạt
1. Một subscriber có channel đầy đúng lúc ghi packet đầu của session mới (viewer RTMP/SRT chậm, đĩa DVR chậm, transcoder bị nghẽn).
2. `select` rơi vào `default`.

Consumer càng chậm thì xác suất càng cao, vì channel của nó thường xuyên đầy.

## Phân tích
```go
if rb.sessionStartPending {
    pkt.SessionStart = true
    rb.sessionStartPending = false
}
for _, s := range rb.subs {
    select { case s.ch <- pc: default: rb.drops.Inc() }
}
```
Latch nằm ở mức ring buffer và bị xóa ngay trong lần ghi đầu, không theo từng subscriber.

Mọi consumer chỉ kiểm tra `pkt.SessionStart`, không có nơi nào so sánh `SessionID` đổi. `packet.go` lại mô tả đây là "the SOLE boundary cue".

## Vì sao đây là lỗi
Invariant không chặn ghi cho phép mất packet media, nhưng tín hiệu ranh giới là trạng thái chứ không phải media. `SessionID` đã được đóng dấu trên mọi packet nhưng không được dùng để khôi phục.

## Hướng sửa
Giữ pending theo subscriber: nếu gửi thất bại thì gắn `SessionStart=true` lên packet kế tiếp gửi được cho subscriber đó. Hoặc consumer so sánh `SessionID` với giá trị trước (lưu ý SessionID=0 ở rendition buffer).

## Cách xác minh
Unit test: `NewServiceForTesting(1)`, subscribe, ghi một packet để đầy channel, `SetSession`, ghi tiếp hai packet, đọc channel rồi ghi thêm: kiểm tra có packet nào `SessionStart=true`. Hiện tại không có.

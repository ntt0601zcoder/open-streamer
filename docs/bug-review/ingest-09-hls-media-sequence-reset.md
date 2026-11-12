# HLS reader bỏ qua mọi segment khi EXT-X-MEDIA-SEQUENCE của upstream reset
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/pull, manager
- **Vị trí:** internal/ingestor/pull/hls.go:257-258, 296-314, 601-603, 631

## Hiện tượng
Sau khi packager phía upstream restart, playlist vẫn trả 200 với segment mới nhưng input ngừng nhận dữ liệu cho tới khi bị timeout. Không có lỗi HTTP nào trong log; chỉ có `no packet for ...`.

## Điều kiện kích hoạt
1. Reader đã giao segment có seq cao, ví dụ `lastSeq = 12000`.
2. Upstream restart và đánh số lại từ `#EXT-X-MEDIA-SEQUENCE:0`, là hành vi mặc định của `ffmpeg -f hls` khi không dùng `append_list` hoặc `start_number`.

## Phân tích
```go
for _, seg := range pl.segments {
    if *started && seg.seq <= *lastSeq {
        continue
    }
```
`started` và `lastSeq` chỉ được khởi tạo trong `poll` và chỉ tăng. Mọi segment mới có seq nhỏ hơn `lastSeq` đều bị bỏ qua. `deliverNewSegments` vẫn trả true, reader tiếp tục poll, không gửi lỗi nào lên.

Reader chỉ thoát khỏi trạng thái này khi seq vượt qua giá trị cũ, với 12000 segment 6 s là khoảng 20 giờ, hoặc khi worker bị thay.

Manager chỉ phát hiện sau packet-timeout. Với một input, tiếp theo là Probe, vốn gần như luôn thất bại với HLS (xem ingest-03), nên worker cũ vẫn kẹt và stream không tự về được.

## Vì sao đây là lỗi
Phần "Deduplication by media-sequence number" ở header giả định sequence đơn điệu suốt đời reader. Upstream restart là tình huống vận hành bình thường. Không có kiểm tra `seqBase` giảm, thẻ `#EXT-X-DISCONTINUITY-SEQUENCE` không được đọc, và `hls_pure_test.go` chỉ test parser.

## Hướng sửa
Lưu `seqBase` của playlist trước. Khi `pl.seqBase` nhỏ hơn đáng kể so với `lastSeq`, hoặc segment cuối có seq nhỏ hơn `lastSeq`, coi là reset: đặt lại `started`/`lastSeq` và bắt đầu lại ở live edge. Cân nhắc trả lỗi nếu playlist không đổi quá N lần target duration.

## Cách xác minh
Unit test với `httptest`: lần đầu playlist seq 100-105, lần sau seq 0-5 với URI khác, kiểm tra `Read` hiện không trả chunk mới. Thủ công: restart `ffmpeg -f hls` upstream và theo dõi `last_packet_at`.

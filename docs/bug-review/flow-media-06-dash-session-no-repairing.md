# DASH không ghép lại A/V sau SessionStart, lệch tiếng được giữ vĩnh viễn
- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** publisher/dash, timeline, transcoder (native)
- **Vị trí:** internal/publisher/dash/packager.go:600-616, 711-714, 1038-1054; internal/publisher/dash/segmenter.go:291-304; internal/timeline/normaliser.go:700-707; internal/transcoder/native/stream_pipeline.go:484-488, 1380-1385

## Hiện tượng
Sau failover hoặc switch input, DASH bị lệch tiếng cố định, từ vài chục đến hàng trăm ms. HLS của cùng stream vẫn đồng bộ. Mức lệch thay đổi sau mỗi lần chuyển nguồn và không tự hết.

## Điều kiện kích hoạt
`SessionStart` đến khi frame video và audio đầu tiên của session mới có PTS khác nhau. Ví dụ:
- Nguồn có offset A/V, joint seed đặt track muộn ở `+|delta|`.
- Transcoder với audio passthrough: audio đã đi trước video tới 500 ms bị xoá cùng queue cũ.

## Phân tích
Căn hai track chỉ chạy một lần, và comment nói rõ không reset sau boundary:
```go
if !p.pairingTruncated && p.state.State() == StateWaitingForPairing && haveVideo && haveAudio {
    p.truncateAtPairingLocked()
```
`onSessionBoundary` đưa state về Live ngay. Sau đó tfdt của mỗi track nối tiếp segment trước, không dùng PTS:
```go
return last.StartTicks + last.DurTicks
```
Số frame audio lấy theo thời lượng video, không theo PTS:
```go
totalSamples := vDurMs*audioSR/1000 + s.audioFrameResidualSamples
```
Do đó frame audio và video đầu queue được đặt vào cùng thời điểm, bất kể PTS chênh bao nhiêu.

## Vì sao đây là lỗi
Comment ở `onSessionBoundary` cho rằng nội dung mới nối vào timeline hiện có, nhưng mốc nối là hai frame có PTS khác nhau. Cold start có `truncateAtPairingLocked` để xử lý đúng tình huống này.

## Hướng sửa
Trong boundary, cho phép chạy lại bước truncate cho session mới (chỉ một lần mỗi session), với cờ riêng để tránh vòng lặp drop mà comment đã nêu.

## Cách xác minh
Test packager: chạy tới Live, gửi `SessionStart`, đẩy video từ PTS 1000 và audio từ PTS 1400, đọc `AudioCount` và kiểm tra audio trước PTS tương ứng bị cắt hoặc được bù.

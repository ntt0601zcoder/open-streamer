# DVR suy wallclock từ PTS, PTS reset làm xoá các giờ đã ghi
- **Mức độ:** Nghiêm trọng
- **Độ tin cậy:** Cao
- **Module liên quan:** dvr/blob, timeline, ingestor (worker), ingestor/push, transcoder
- **Vị trí:** internal/dvr/blob/writer.go:103-111, 231-235, 329-331, 376-387, 389-403; internal/ingestor/worker.go:88, 96; internal/ingestor/push/rtmp_server.go:452; internal/transcoder/native/stream_pipeline.go:380-393, 642-653

## Hiện tượng
Stream có DVR đang ghi qua ranh giới giờ. Encoder RTMP kết nối lại hoặc input failover. Từ đó các giờ đã ghi lần lượt bị xoá khỏi đĩa. Log có `blob: resuming into existing hour; discarding partial hour`. Timeshift mất đoạn cũ, và đoạn đang phát mới bị gắn nhãn thời gian của quá khứ.

## Điều kiện kích hoạt
1. Stream không transcode, nguồn AV (`publish://`, RTMP/RTSP pull, `copy://`), DVR bật. Ví dụ bắt đầu ghi lúc 10:50.
2. Lúc 12:10 encoder đẩy lại, hoặc manager failover sang worker mới. Normaliser mới có `wallOrigin` mới nên PTS quay về khoảng 0.
3. Biến thể transcode: subprocess respawn, PTS rendition cũng về khoảng 1 ms và không có `SessionStart` (xem flow-media-02).

## Phân tích
Mỗi worker và mỗi publish session tạo Normaliser riêng, nên PTS không liên tục qua failover hay publisher mới. Writer lại coi PTS là liên tục cho cả đời recording:
```go
wallMs := w.originWallMs + w.deltaMs(firstPTS)
if err := w.ensureHour(wallMs); err != nil {
```
Khối `pkt.SessionStart` trong `Ingest` chỉ thay queue, reset segmenter và đặt `pendingDisc`. Nó không đổi `originPTSms` hay `originWallMs`.

Với PTS gần 0, `wallMs` rơi về 10:50, nên `ensureHour` seal giờ 12 rồi `openHour(10:00)`. Hàm này xoá file đã có:
```go
if hourFilesExist(stem) {
    slog.Warn("blob: resuming into existing hour; discarding partial hour", ...)
    removeHourFiles(stem)
}
```
Khi PTS mới tăng qua mốc 11:00, giờ 11 bị xoá theo cách tương tự. Giờ 12 đến lượt sau đó. `upsertHour` cũng ghi đè bản ghi trong catalog.

## Vì sao đây là lỗi
Nhánh xoá được viết cho trường hợp restart giữa giờ, khi writer vừa được tạo. Writer đang chạy không có guard nào ngăn thời gian lùi. `SessionStart` đến đúng lúc nhưng không được dùng để neo lại timeline.

## Hướng sửa
Khi có `SessionStart`, hoặc khi PTS lùi xa, neo lại `originPTSms` và `originWallMs` theo `now`. `openHour` không được xoá giờ đã seal trong cùng phiên ghi; nên ghi thêm hoặc dùng stem mới.

## Cách xác minh
Unit test `profileWriter`: ingest qua một mốc giờ bằng clock giả, gửi packet `SessionStart` với PTS 0 và IDR hợp lệ, rồi kiểm tra file của giờ đầu còn nguyên và `FragRecord.WallTimeMs` tăng dần.

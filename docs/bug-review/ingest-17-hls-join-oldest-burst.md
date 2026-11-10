# HLS pull bắt đầu từ segment cũ nhất và đẩy dồn cả playlist không pacing
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/pull, ingestor, timeline
- **Vị trí:** internal/ingestor/pull/hls.go:296-315; internal/ingestor/reader.go:104-110; internal/domain/defaults.go:119

## Hiện tượng
Khi kết nối playlist live có cửa sổ dài, stream xuất ra trễ đúng bằng độ dài cửa sổ, ví dụ playlist DVR 1 giờ thì người xem trễ khoảng 1 giờ, và độ trễ không giảm. Mỗi lần reconnect lại nhảy về nội dung cũ. Input VOD HLS bị tải và đẩy toàn bộ với tốc độ mạng.

## Điều kiện kích hoạt
Input `.m3u8` live có nhiều segment hoặc `EVENT`, hoặc VOD có `#EXT-X-ENDLIST`.

## Phân tích
Lần poll đầu không lọc gì vì `started` là false, nên toàn bộ danh sách được tải theo thứ tự:
```go
for _, seg := range pl.segments {
    if *started && seg.seq <= *lastSeq { continue }
    data, err := r.fetchSegmentWithRetry(ctx, seg.uri)
```
Comment ở `reader.go` nói pacing được bỏ vì "the segmenter/transcoder downstream already paces by wall-clock", nhưng đường raw-TS không có pacing đó. Normaliser so drift với `max(actualNow, lastOutputDts)` và `MaxAheadMs = 0`, nên timeline output vượt wallclock theo toàn bộ phần burst và giữ vậy.

Vòng reconnect cũng tạo `poll` mới với `started = false`, nên quay về đầu cửa sổ và phát lại nội dung đã phát.

## Vì sao đây là lỗi
RFC 8216 khuyến nghị client bắt đầu cách cuối playlist ít nhất ba target duration với live. Pipeline là live: output dựa trên wallclock và không có khái niệm tua. `hls_pure_test.go` chỉ phủ parser.

## Hướng sửa
Với playlist không có `ENDLIST`, khởi đầu ở N segment cuối (khoảng 3). Thêm pacing theo `EXTINF`/DTS cho VOD. Giữ `lastSeq` qua reconnect trong cùng reader.

## Cách xác minh
Unit test với `httptest` phát playlist live 100 segment: đếm số GET segment trong lần đầu. Thủ công: pull từ một playlist cửa sổ dài và so nội dung với live edge.

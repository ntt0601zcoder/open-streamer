# EXTINF đường raw-TS theo wallclock, sai lệch lớn với nguồn HLS đến theo từng đợt
- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** publisher (HLS), ingestor/pull (HLS), ingestor/tsnorm
- **Vị trí:** internal/publisher/hls.go:598-602, 635-657, 680-686, 782-788; internal/ingestor/pull/hls.go:300-311

## Hiện tượng
Stream HLS-pull không transcode có segment output dài ngắn thất thường, nhiều segment chứa media dài gấp 2-3 lần `EXTINF`. Player có thể giật ở ranh giới, ước lượng buffer sai, hoặc báo segment vượt `EXT-X-TARGETDURATION`.

## Điều kiện kích hoạt
Input HLS không transcode, segment nguồn dài hơn `segSec` output. Ví dụ nguồn 6 giây, GOP 2 giây, output `segSec=2`.

## Phân tích
Reader giao cả segment trong một lần, nên 6 giây media vào `segBuf` gần như cùng lúc. Sau 2 giây wallclock, tick cắt tại IDR mới nhất:
```go
if p.scanner != nil && elapsed >= segDur {
    if p.tryFlushAtIDRLocked() {
```
`LastIDROffset` là IDR gần cuối đợt, nên segment chứa khoảng 4 giây media. Nhưng thời lượng lại lấy từ wallclock vì đường raw-TS không có `hasMediaStart`:
```go
dur := time.Since(p.segStart).Seconds()
```
`EXTINF` khoảng 2 giây cho 4 giây media. Đợt sau lại lệch theo hướng ngược lại. `targetDur` cũng tính từ các giá trị này, nên không phản ánh segment dài nhất.

## Vì sao đây là lỗi
Sau refactor, tsnorm đảm bảo chunk có PTS chuẩn nên publisher đọc được thời gian media. Comment ở `recordMediaTime` đã nêu vấn đề của wallclock trên buffer hub bị burst, nhưng chỉ sửa đường AV. tick cũng cắt theo IDR cuối thay vì IDR đầu vượt `segDur`, điều DASH đã sửa vì lý do tương tự.

## Hướng sửa
Scanner đọc PTS của PES video để tính thời lượng, và chọn IDR đầu tiên vượt `segDur` thay vì IDR cuối.

## Cách xác minh
Test segmenter raw-TS: feed một chunk 6 giây có IDR mỗi 2 giây, giả lập 2 giây wallclock, so `EXTINF` với khoảng PTS của file segment. Thực tế: ffprobe segment output và so với playlist.

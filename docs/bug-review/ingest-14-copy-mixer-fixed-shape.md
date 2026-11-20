# copy:// và mixer:// cố định kiểu buffer theo Inputs[0], mất tín hiệu khi upstream failover khác loại
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/pull, domain, buffer
- **Vị trí:** internal/ingestor/pull/copy.go:102-105, 153-157; internal/ingestor/pull/buffer_ts_chunk.go:82-87; internal/ingestor/pull/mixer.go:149-156, 284-305; internal/domain/copy_shape.go:181-192

## Hiện tượng
Stream `copy://A` hoặc `mixer://A,B` im lặng ngay khi A chuyển sang input khác protocol, mặc dù A vẫn phát bình thường. Stream hạ nguồn bị timeout và Probe cũng thất bại cho tới khi A quay lại input cũ.

## Điều kiện kích hoạt
Upstream không có transcoder với inputs khác kiểu: p0 = `rtsp://cam`, p1 = `https://backup/index.m3u8` (AV và raw-TS), hoặc ngược lại. Upstream failover. Cũng xảy ra khi `Inputs[0]` không phải input có priority thấp nhất.

## Phân tích
Kiểu buffer được đoán một lần từ cấu hình:
```go
return inputSourceIsRawTS(s.Inputs[0].URL)
```
Chế độ đọc được chọn lúc tạo reader và không đổi. Ở chế độ AV, gói TS bị bỏ:
```go
if pkt.AV == nil {
    return nil, nil
}
```
Ở chế độ TS, gói AV cũng bị bỏ trong `bufferTSChunkReader.Read` (`len(pkt.TS) == 0` → `continue`). Reader không báo lỗi. `Probe` dựng reader với cùng kiểu nên nhận 0 payload.

## Vì sao đây là lỗi
Hub không đảm bảo một kiểu payload: comment ở `copy.go` cảnh báo chính việc phân loại sai "silently drops the entire stream". Đổi nguồn lúc chạy là đúng chức năng chính của manager. Reader không phản ứng với `pkt.SessionStart`, là thời điểm kiểu có thể thay đổi.

## Hướng sửa
Xử lý theo từng packet: nếu có `pkt.AV` thì dùng trực tiếp, nếu có `pkt.TS` thì đưa vào demuxer nội bộ. Reset demuxer khi `SessionStart`. Bỏ phụ thuộc vào `Inputs[0]`.

## Cách xác minh
Unit test với `buffer.NewServiceForTesting`: dựng `CopyReader` cho upstream RTSP, ghi `Packet{TS: ...}`, kiểm tra không có packet ra. Thủ công: upstream hai input RTSP + HLS, `copy://`, tắt RTSP và xem output hạ nguồn.

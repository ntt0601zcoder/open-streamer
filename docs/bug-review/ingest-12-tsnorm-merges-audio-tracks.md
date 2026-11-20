# tsnorm gộp mọi track cùng codec vào một PID, feed đa ngôn ngữ bị trộn audio
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/tsnorm, ingestor/pull, timeline
- **Vị trí:** internal/ingestor/tsnorm/tsnorm.go:97-101, 425-435, 450-458, 469-492; internal/ingestor/reader.go:95-103, 162-167

## Hiện tượng
Nguồn TS có từ hai track AAC trở lên, ví dụ hai ngôn ngữ, cho ra một track audio bị trộn: tiếng nhanh gấp đôi, xen kẽ hai ngôn ngữ, hoặc lỗi giải mã. Tương tự, MPTS không đặt `program` thì video các chương trình bị gộp vào một PID.

## Điều kiện kích hoạt
Input raw-TS không đi qua `PIDFilter` và PMT có hơn một PID cùng loại, ví dụ một video H.264 và hai PID AAC. Với UDP nhiều chương trình, `input.Program == 0` là mặc định.

## Phân tích
Mọi PID trong PMT đều được giữ lại:
```go
for _, es := range data.PMT.ElementaryStreams {
    n.pidStream[es.ElementaryPID] = es.StreamType
}
```
`handlePES` chỉ dùng stream type, sau đó `writeMuxed` ánh xạ theo codec:
```go
case domain.AVCodecAAC:
    pid = audioPID
```
Không có logic chọn track đầu tiên hoặc phân biệt PID nguồn. Hai PES cùng thời điểm từ hai PID đều vào cùng output PID và cùng trackState audio trong Normaliser. Normaliser coi DTS bằng nhau là hợp lệ và chỉ re-anchor khi lùi, nên không có gì lọc được.

## Vì sao đây là lỗi
Trước khi có tsnorm, PID và PMT gốc được giữ nguyên: `ts_passthrough.go` nói rõ về tránh "re-assigns PIDs". Workaround `pids` đòi operator biết PID cụ thể; kênh đa ngôn ngữ là mặc định trong nhiều headend. `tsnorm_test.go` không có trường hợp nhiều PID cùng codec.

## Hướng sửa
Khoá một PID cho mỗi loại track (PID đầu tiên trong PMT, hoặc ánh xạ theo input config), bỏ các PID còn lại kèm log, hoặc tạo PID output riêng cho từng track. Với MPTS không chọn program, chỉ lấy chương trình đầu tiên như `statsPMTScanner.parsePAT`.

## Cách xác minh
Unit test: TS tổng hợp có hai PID AAC trong cùng PMT, kiểm tra số PES AAC ra bằng tổng cả hai. Thủ công: `ffmpeg -map 0:v -map 0:a -map 0:a -f mpegts udp://...`, `ffprobe` đầu ra và nghe thử.

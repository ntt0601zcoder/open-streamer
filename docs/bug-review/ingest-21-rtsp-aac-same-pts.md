# RTSP gán cùng PTS cho mọi AAC access unit trong một gói RTP
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/pull, timeline
- **Vị trí:** internal/ingestor/pull/rtsp.go:418-441; internal/timeline/normaliser.go:514

## Hiện tượng
Nguồn RTSP gom nhiều AU AAC vào một gói RTP tạo các frame audio có PTS trùng nhau, rồi bước nhảy lớn ở gói kế tiếp. Có thể gây jitter audio và sai thời lượng khi player hoặc re-stream dùng PTS từng frame, nhất là RTSP/RTMP re-stream.

## Điều kiện kích hoạt
Server RTSP gửi AAC-hbr aggregate với hơn một AU mỗi gói.

## Phân tích
```go
ptsMS := pts * 1000 / clockRate
for _, au := range aus {
    ...
    PTSms: uint64(ptsMS),
    DTSms: uint64(ptsMS),
```
`pts` là thời điểm của AU đầu. Theo RFC 3640, AU thứ i cách `i × 1024` mẫu. Normaliser không sửa vì DTS bằng nhau không tính là `regressed` (`expectedDts < track.lastOutputDts`).

## Vì sao đây là lỗi
`push/rtmp_writer.go` xử lý đúng bài toán tương tự bằng offset `frameIndex × 1024 × 1000 / sampleRate`, còn đường RTSP thì không.

## Hướng sửa
Tính `ptsMS` theo chỉ số AU: `(pts + i*mpeg4audio.SamplesPerAccessUnit) * 1000 / clockRate`.

## Cách xác minh
Unit test với decoder trả nhiều AU, kiểm tra PTS tăng dần. Thủ công: server RTSP thử nghiệm phát `aac` aggregate, dump PTS của track audio.

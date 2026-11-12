# RTMP push gửi inter-frame trước AVC sequence header sau mỗi lần kết nối
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (RTMP push)
- **Vị trí:** internal/publisher/push_codec.go:128-155; internal/publisher/push_rtmp.go:330-354

## Hiện tượng
Sau khi kết nối hoặc reconnect, đích nhận các tag video P/B trước config AVC. Đích chặt có thể báo lỗi, ngắt kết nối hoặc hiển thị hình vỡ đến IDR kế tiếp.

## Điều kiện kích hoạt
Bất kỳ phiên push nào bắt đầu giữa GOP: reconnect sau lỗi mạng, reconnect do `errDiscontinuity`, hoặc bật push trên stream đang chạy. Khi đó SPS/PPS chưa có vì chúng chỉ đi cùng IDR.

## Phân tích
`FeedH264` chỉ phát sequence header khi đã có SPS/PPS, nhưng vẫn gửi slice dù chưa có:
```go
if !a.seqHeaderSet && len(a.sps) > 0 && len(a.pps) > 0 { ... }
if len(slices) == 0 { return nil }
payload := buildAVCNaluTag(slices, isKey, int32(cts))
return a.emitVideo(payload, uint32(dtsMs))
```
`onTSFrame` cũng đặt `baseDTS` theo frame đầu, không chờ IDR.

## Vì sao đây là lỗi
Comment ở đầu file nói sequence header là tag video đầu tiên và các cặp RTMP play/RTSP đều chờ IDR (`firstVideo`). Riêng push không có cổng này. Adapter mới tạo cho mỗi phiên nên trường hợp này lặp lại ở mỗi reconnect.

## Hướng sửa
Bỏ video cho đến IDR đầu tiên có sequence header. Chờ IDR rồi mới cho audio và đặt `baseDTS`, như RTMP play.

## Cách xác minh
Unit test: `FeedH264` với P-frame trước IDR không được phát tag NALU. Thực tế: cắt mạng tới đích trong lúc push, kiểm tra thứ tự tag ở server nhận.

# RTMP play dùng hai mốc thời gian riêng cho video và audio
- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** publisher (serve_rtmp), transcoder (native)
- **Vị trí:** internal/publisher/serve_rtmp.go:285-293, 301-309; internal/publisher/push_rtmp.go:330-333; internal/transcoder/native/stream_pipeline.go:259-260, 484-488; internal/timeline/normaliser.go:694-708

## Hiện tượng
Client RTMP bị lệch tiếng so với hình một khoảng cố định, khác nhau giữa các lần kết nối. HLS của cùng stream vẫn đồng bộ.

## Điều kiện kích hoạt
Client bắt đầu play khi audio đầu tiên sau IDR có DTS khác DTS của IDR đó. Thường gặp với stream transcode: audio có thể đi trước video tới `audioLeadCapMs` (500 ms). Cũng gặp với nguồn có offset A/V nội tại.

## Phân tích
Video và audio được đưa về 0 độc lập:
```go
// writeVideo
ps.baseDTS = dts
// writeAudio
if !ps.baseADTSSet {
    ps.baseADTS = dts
    ps.baseADTSSet = true
}
relDTS := uint32(dts - ps.baseADTS)
```
Nếu IDR có DTS D và audio đầu tiên có DTS D+400, cả hai đều thành timestamp 0, nên audio phát trễ 400 ms so với nội dung gốc. Độ lệch giữ nguyên suốt phiên.

## Vì sao đây là lỗi
Normaliser giữ offset A/V của nguồn, và transcoder chủ ý chỉ giữ audio ở mức dưới 500 ms. RTMP play xoá offset đó. Push RTMP cùng package dùng một `baseDTS` chung nên không bị.

## Hướng sửa
Dùng một mốc chung từ IDR đầu tiên. Audio có DTS nhỏ hơn mốc thì bỏ hoặc kẹp về 0.

## Cách xác minh
Unit test: gọi `onTSFrame` với IDR DTS 1000, AAC DTS 1400, rồi kiểm tra timestamp audio là 400. Hiện tại là 0.

# RTMP push gửi nguyên PES AAC nhiều frame thành một tag, làm hỏng audio
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (RTMP push), tsdemux
- **Vị trí:** internal/publisher/push_rtmp.go:350-351; internal/publisher/push_codec.go:162-188; internal/tsdemux/demuxer.go:131-132

## Hiện tượng
Đích RTMP nhận audio méo, thiếu hoặc lỗi giải mã. Thời lượng audio bên nhận ngắn hơn thực tế theo hệ số gộp frame (thường 4-8 lần).

## Điều kiện kích hoạt
Stream raw-TS (UDP, SRT, HLS pull, HTTP-TS, file) có PES AAC chứa nhiều frame ADTS, rồi cấu hình push `rtmp://`. ffmpeg và nhiều encoder broadcast mặc định gộp như vậy.

## Phân tích
`tsdemux` trả nguyên payload PES. `onTSFrame` chuyển thẳng vào `FeedAAC`, và hàm này giả định chỉ có một frame:
```go
raw := adtsFrame[adtsHeaderSize:]
payload := make([]byte, 2+len(raw))
```
Chỉ header ADTS đầu bị bỏ. Các header ADTS sau và các frame sau nằm lẫn trong một raw AAC access unit, cùng một timestamp.

## Vì sao đây là lỗi
ARCHITECTURE ghi rõ PES AAC gộp 4-8 frame, và RTMP play đã tách (`RTMPFrameWriter.sendAACBundle`), DASH cũng tách (`splitADTSBundle`). Riêng push không tách. Test `TestFeedAAC_*` chỉ dùng một frame.

## Hướng sửa
Duyệt các frame ADTS trong `FeedAAC` như `sendAACBundle`, mỗi frame một tag với DTS = base + i x 1024 x 1000 / sampleRate. Đồng thời dùng `HeaderLength` thật thay vì hằng 7 để xử lý ADTS có CRC.

## Cách xác minh
Unit test: `FeedAAC` với hai frame ADTS nối nhau phải phát sequence header và hai tag raw, DTS tăng. Thực tế: push tới một server cục bộ từ nguồn TS có `-pes_payload_size` lớn, so sánh số sample audio bằng ffprobe.

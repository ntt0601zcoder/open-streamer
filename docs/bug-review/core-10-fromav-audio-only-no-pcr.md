# FromAV không phát PCR cho stream chỉ có audio
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** tsmux
- **Vị trí:** internal/tsmux/fromav.go:125-130,178-192

## Hiện tượng
Output TS của stream chỉ có audio (radio qua RTMP/RTSP pull) không có PCR nào. Các client dựa vào PCR (VLC, thiết bị phần cứng, bộ phân tích TS) báo thiếu PCR hoặc timing không ổn định.

## Điều kiện kích hoạt
AVPacket chỉ có audio đi qua `FromAV`, ví dụ HLS hoặc phát SRT/MPEG-TS từ buffer AV.

## Phân tích
PMT khai báo PCR PID là audio:
```go
if !f.hasV { f.mux.SetPCRPID(pid) }
```
Nhưng adaptation field có PCR chỉ tạo khi `isVideo`; nhánh audio chỉ đặt `DiscontinuityIndicator` ở packet đầu.

## Vì sao đây là lỗi
Comment trong chính hàm trích dẫn yêu cầu PCR dưới 100 ms theo H.222.0. Lý do "PCR rides on video" không còn đúng khi PCR PID đã được đặt là audio. Comment "audio-only pipelines rely on the retransmit period" chỉ nói về PSI.

## Hướng sửa
Khi `!f.hasV`, phát PCR trên audio PES theo DTS. Cần xử lý trường hợp video đến sau: chuyển PCR PID và thôi phát PCR trên audio.

## Cách xác minh
Write nhiều AAC packet vào `FromAV` mới, parse TS: kiểm tra có adaptation field có PCR trên PID 0x101.

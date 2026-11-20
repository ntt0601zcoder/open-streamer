# Buffer UDP mặc định 1316 byte cắt cụt datagram RTP/MPEG-TS
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/pull, ingestor/tsnorm
- **Vị trí:** internal/ingestor/pull/udp.go:58, 232, 242, 257, 287-293, 315-346, 427-429

## Hiện tượng
Nguồn multicast/unicast dùng RTP bọc MPEG-TS, kiểu rất phổ biến ở headend IPTV, được nhận và báo Active nhưng hình vỡ hoặc không xem được, kèm log `tsnorm demux failed, falling back to passthrough`. Thêm `?pkt_size=1500` vào URL thì hết lỗi.

## Điều kiện kích hoạt
Input `udp://...` không đặt `pkt_size`; mỗi datagram gồm RTP header 12 byte và 7 gói TS, tổng 1328 byte.

## Phân tích
Buffer nhận bằng đúng phần payload TS, không tính header RTP:
```go
udpDefaultPktSize = 188 * 7
...
buf := make([]byte, r.opts.pktSize)
n, _, err := r.conn.ReadFromUDP(buf)
```
Trên Linux, `ReadFromUDP` với buffer nhỏ hơn datagram trả `n = 1316` và không báo lỗi; 12 byte cuối bị kernel bỏ. `stripRTPHeader` bỏ 12 byte đầu, còn 1304 byte tức 6 gói đủ và gói thứ 7 thiếu 12 byte.

Chunk này vẫn được đẩy tiếp. Trong tsnorm, `chunkReader` nối các chunk thành luồng byte liên tục và astits đọc đúng 188 byte một lần, nên từ datagram kế tiếp stream lệch sync. Mỗi datagram lệch thêm 12 byte, demuxer lỗi liên tiếp, goroutine chết sau `maxDemuxRestarts`, chunk rơi về passthrough thô. Gói TS thứ 7 của mọi datagram đều hỏng.

## Vì sao đây là lỗi
Tính năng tự động bỏ RTP được quảng bá trong header `udp.go`, và `rtpAuto` mặc định true. Test `udp_internal_test.go` chỉ gọi `stripRTPHeader` trực tiếp trên slice đầy đủ, không đi qua socket với buffer mặc định. Không có kiểm tra độ dài payload là bội của 188.

## Hướng sửa
Đặt buffer đọc mặc định bằng `udpMaxPktSize` (65536), tách khỏi kích thước chunk. Dùng `ReadMsgUDP` để phát hiện `MSG_TRUNC` khi người dùng đặt `pkt_size` nhỏ. Chỉ forward số nguyên lần 188 byte.

## Cách xác minh
Test socket loopback: gửi datagram RTP 1328 byte tới `UDPReader`, kiểm tra `Read` hiện trả 1304 byte thay vì 1316. Thủ công: `ffmpeg ... -f rtp_mpegts udp://...` tới input, so sánh với và không có `pkt_size=1500`.

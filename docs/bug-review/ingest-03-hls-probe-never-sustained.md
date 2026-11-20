# Probe tính mỗi segment HLS là một packet nên input HLS gần như không thể phục hồi
- **Mức độ:** Nghiêm trọng
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor, ingestor/pull, manager
- **Vị trí:** internal/ingestor/service.go:373-385, 395-407, 426-453; internal/ingestor/pull/ts_passthrough.go:63-75; internal/ingestor/pull/hls.go:296-315; internal/manager/service.go:50, 870-889, 1052

## Hiện tượng
Khi một input HLS bị Degraded, các probe sau đó đều thất bại dù nguồn đã hoạt động lại. Stream chỉ có một input HLS sẽ kẹt ở trạng thái Exhausted vĩnh viễn nếu worker đã thoát. Stream có dự phòng thì không bao giờ failback về HLS chính. Log chỉ tăng `probe failure` với thông báo `probe source not sustained`.

## Điều kiện kích hoạt
1. Input HLS có playlist dưới 10 segment (thường là 3 đến 6), hoặc có đủ segment nhưng tải rất nhanh trên LAN.
2. Input bị Degraded, ví dụ playlist trả HTTP 502 sau 5 lần thử: `shouldFailoverImmediately` true, worker thoát.
3. Nguồn sống lại, manager chạy `Probe` với `probeTimeout = 5s`.

## Phân tích
Ngưỡng probe:
```go
minProbeSpan    = 1500 * time.Millisecond
minProbePackets = 10
minProbeBytes   = 16 * 1024
```
Với nguồn raw-TS, `TSPassthroughPacketReader` trả một `AVPacket` cho mỗi chunk. `HLSReader` phát một chunk cho cả segment, nên `probeStats.packets` chính là số segment. Trong 5 s, playlist 5 segment cho nhiều nhất 5 hoặc 6 packet, không đạt 10.

Nếu playlist dài hơn, cả 10 segment được tải dồn trong một burst, `firstAt` và `lastAt` sát nhau, không đạt span 1,5 s. Lần poll kế tiếp phải đợi `targetDuration/2` và segment mới, thường đã gần hoặc quá hạn 5 s.

Khi hết hạn, `ReadPackets` trả lỗi context và `Probe` trả `notSustainedErr`. Vòng lặp lặp lại mãi.

## Vì sao đây là lỗi
Ngưỡng được thiết kế cho nguồn gửi đều (RTMP, RTSP, UDP) và đơn vị "packet" khác hẳn với HLS. Khi Exhausted, worker cũ đã thoát, nên `RecordPacket` không có nguồn nào để tự phục hồi. Reconciler thấy pipeline vẫn `running` nên không Start lại. `probe_test.go` chỉ test hàm thuần với số liệu giả, không đi qua `HLSReader`.

## Hướng sửa
Với nguồn dạng chunk, tính sustained theo thời lượng media (`#EXTINF` hoặc PTS đã đọc) hoặc chấp nhận một lần tải playlist và segment thành công. Có thể cho `PacketReader` khai báo policy probe riêng, hoặc tăng timeout cho HLS theo target duration.

## Cách xác minh
Test `Probe` với `httptest` phát playlist live 5 segment, segment 6 s: hiện tại trả lỗi. Thủ công trên môi trường thử: một input HLS, cho upstream trả 502 đủ lâu để worker thoát, khôi phục upstream, theo dõi `manager_probe_attempts_total{result="failure"}` tăng mãi và stream không về Active.

# Failover khác loại nguồn làm copy://, mixer:// và DVR mất dữ liệu

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/domain, internal/manager, internal/ingestor/pull, internal/coordinator, internal/dvr/blob
- **Vị trí:** internal/domain/copy_shape.go:181-192, internal/ingestor/pull/copy.go:102-105, internal/ingestor/pull/copy.go:153-157, internal/ingestor/pull/buffer_ts_chunk.go:83-88, internal/ingestor/pull/mixer.go:149-156, internal/coordinator/abr_mixer.go:223, internal/coordinator/coordinator.go:770-775, internal/dvr/blob/writer.go:117-122

## Hiện tượng
Upstream có `inputs = [rtmp (0), hls (1)]`: khi upstream failover, viewer của chính nó vẫn xem bình thường, nhưng downstream `copy://`/`mixer://` mất dữ liệu và DVR ngừng ghi trong khi vẫn báo đang ghi. Downstream exhausted cho tới khi upstream quay về input đầu.

Chiều ngược lại (`[hls, rtmp]`) cũng vậy với consumer chế độ TS, và DVR bị từ chối ngay từ đầu dù input khác có thể ghi được.

## Điều kiện kích hoạt
Stream không transcode có các input thuộc hai họ: AV-path (RTMP, RTSP, `publish://`) và raw-TS (UDP, HLS, HTTP-TS, SRT, file). Manager chuyển giữa hai họ khi failover hoặc manual switch.

## Phân tích
Dạng buffer chỉ suy ra từ input đầu, không theo input đang active:
```go
return inputSourceIsRawTS(s.Inputs[0].URL)
```
- `CopyReader` chế độ direct bỏ gói TS: `if pkt.AV == nil { return nil, nil }`.
- Chế độ TS bỏ gói AV (`buffer_ts_chunk.go`).
- Probe của downstream dùng cùng phân loại nên cũng không có payload và không phục hồi được.
- `blobProfiles` chốt theo `Inputs[0]`, còn writer bỏ `pkt.TS` ("ingress lands ... in a follow-up").

`failover` thật sự đổi dạng gói vì mỗi worker ghi theo reader riêng.

## Vì sao đây là lỗi
Publisher xử lý theo từng packet nên không bị. Comment B-7 nhấn mạnh phân loại sai làm blackout, nhưng hàm vẫn giả định cả stream chỉ có một dạng. `ValidateCopyShape` không cấm upstream trộn họ input.

## Hướng sửa
Cho consumer xử lý theo từng packet (`Packet.AV` hoặc `Packet.TS`) thay vì chốt chế độ khi tạo reader, và DVR ingest cả `pkt.TS`. Nếu chưa làm được, chặn cấu hình trộn họ input khi upstream được copy/mixer/DVR.

## Cách xác minh
Unit test `CopyReader`: mở theo upstream có `Inputs[0]=rtmp`, ghi `Packet{TS: ...}` vào buffer, khẳng định có dữ liệu ra. Test DVR writer tương tự với `pkt.TS`.

# Worker cũ ghi sau SetSession của worker mới và chiếm cờ SessionStart
- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** ingestor, ingestor/tsnorm, buffer, publisher
- **Vị trí:** internal/ingestor/worker.go:96, 99-112, 348-362, 382-406; internal/ingestor/service.go:577-585; internal/ingestor/tsnorm/tsnorm.go:317-342; internal/buffer/service.go:63-69, 330-333; internal/publisher/hls.go:311-315

## Hiện tượng
Tại lúc failover hoặc đổi input, ranh giới session có thể gắn vào gói cuối của nguồn cũ. Gói đầu của nguồn mới không có `SessionStart`, trong khi DTS của nó quay về gần 0. Segment HLS có timestamp lùi mà không có `#EXT-X-DISCONTINUITY` đúng chỗ; DASH và RTMP/RTSP re-stream giữ state cũ qua boundary, tới khi xuất hiện session kế tiếp.

Đây là cơ chế có thể góp phần vào cảnh báo OOO đã biết, nhưng lỗi cần xử lý là cờ boundary bị mất, không phải bản thân log.

## Điều kiện kích hoạt
Failover có handoff giữa hai pull worker, hoặc thay đổi cấu hình cùng priority. Với nguồn raw-TS, chỉ cần worker cũ đang giữ PES dở dang, đây là trạng thái thường trực.

## Phân tích
Worker mới huỷ worker cũ rồi lập tức mint session:
```go
err := r.Open(ctx); if err == nil { fireHandoffOnce(cb, handedOff); return true }
...
mintSessionForOpen(streamID, bufferWriteID, buf, &cb, firstOpenDone, norm, tsNorm)
```
Worker cũ thức dậy từ `ReadPackets` và defer của `readLoop` ghi phần tail:
```go
if tail := tsNorm.Flush(); len(tail) > 0 {
    _ = buf.Write(bufferWriteID, buffer.Packet{TS: tail})
}
```
Trên AV path, vòng `for _, p := range batch` không kiểm tra ctx, nên có thể ghi nốt tối đa 256 gói còn lại.

Hub stamp cờ một lần cho packet kế tiếp, bất kể ai ghi:
```go
if rb.sessionStartPending {
    pkt.SessionStart = true
    rb.sessionStartPending = false
}
```
Trong khi worker mới còn phải đợi mạng để có gói đầu, write của worker cũ thắng race với xác suất cao. `tsnorm.New` và `timeline.New` tạo mới theo worker nên DTS của worker mới bắt đầu lại từ mốc gần 0.

## Vì sao đây là lỗi
`Flush` ở `tsnorm` kết thúc ở `<-n.demuxDone` nên chắc chắn hoàn tất tail; astits dump pool khi reader trả EOF. Không có khoá hoặc session token nào ngăn writer cũ. Comment của `Buffer.Write` yêu cầu chỉ worker active được ghi. `publisher/hls.go:311` và DASH dùng `SessionStart` làm tín hiệu duy nhất.

## Hướng sửa
Đợi worker cũ thoát hẳn (channel `done`) trước khi mint session, hoặc bỏ tail khi `ctx` đã huỷ và kiểm tra `ctx.Err()` trong vòng batch. Bền hơn là cấp token writer và để `Write` từ chối token cũ.

## Cách xác minh
Test tăng cường: reader giả trả batch lớn, huỷ ctx giữa chừng, `SetSession` từ goroutine khác; kiểm tra packet mang `SessionStart=true` có thuộc nguồn mới không. Trên môi trường thử: log `SessionStart` kèm nguồn và chạy nhiều lần failover UDP sang UDP.

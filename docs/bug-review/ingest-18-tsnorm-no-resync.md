# tsnorm không thực sự resync khi lệch căn 188 byte; .m2ts và HTTP-TS lệch biên hỏng vĩnh viễn
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/tsnorm, ingestor/pull, thư viện astits
- **Vị trí:** internal/ingestor/tsnorm/tsnorm.go:114, 387-421, 566-603; internal/ingestor/worker.go:553-559; internal/ingestor/pull/file.go:7, 104-110; internal/ingestor/pull/httpts.go:115-118; go-astits v1.15.0 packet_buffer.go `next`, packet.go:88-92

## Hiện tượng
File `.m2ts` dạng BDAV, hoặc HTTP-TS mà luồng byte không bắt đầu đúng biên gói, không được normalise. Log lặp `tsnorm demux failed, falling back to passthrough` cho mỗi chunk và `demuxer giving up`, output là byte thô chưa anchor. Trạng thái này không tự hết.

## Điều kiện kích hoạt
- Input `file://.../x.m2ts` với gói 192 byte. Header `file.go` liệt kê `.m2ts` là được hỗ trợ.
- HTTP-TS upstream phát từ giữa gói, hoặc có một byte lạ trong luồng TCP.

## Phân tích
Packet size bị ghim và astits đọc đúng 188 byte, không dò sync:
```go
astits.DemuxerOptPacketSize(tsPacketSize)
```
`parsePacket` trả `ErrPacketMustStartWithASyncByte` nếu byte đầu không phải `0x47`. Vòng lặp "resync" chỉ dựng lại demuxer trên cùng reader, đọc tiếp 188 byte ở cùng độ lệch:
```go
slog.Debug("tsnorm: demuxer parse error — rebuilding to resync", ...)
dmx = astits.NewDemuxer(n.ctx, rdr, astits.DemuxerOptPacketSize(tsPacketSize))
```
Sau 9 lỗi goroutine thoát và `rdr.rem` bị bỏ. `Process` khởi động goroutine mới ở chunk sau, với chunkReader mới bắt đầu ngay đầu chunk. UDP/SRT có biên datagram đúng nên tự hồi phục. HTTP-TS có biên `Body.Read` bất kỳ và M2TS luôn lệch 4 byte, nên không thoát được; mỗi chunk đều rơi về passthrough.

## Vì sao đây là lỗi
Comment ghi "rebuild the demuxer on the SAME reader so it resyncs on the next sync byte", không đúng với astits khi ghim packet size. `MPTSProgramFilter` và `PIDFilter` có `firstSync`, còn tsnorm thì không.

## Hướng sửa
Thêm lớp căn chỉnh trước astits: tìm `0x47` lặp lại theo chu kỳ 188/192/204, cắt prefix M2TS. Thay vì restart, tiếp tục trên luồng đã căn lại.

## Cách xác minh
Unit test: nạp TS hợp lệ có thêm 3 byte đầu vào `Process` nhiều chunk, kiểm tra output hiện trống/EOF. Thủ công: tạo `.m2ts` 192 byte bằng `tsMuxeR` hoặc `ffmpeg -mpegts_m2ts_mode 1`, phát qua `file://`.

# Một lỗi parse TS làm output DASH hoặc RTSP raw-TS dừng tới khi restart stream
- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** publisher/dash, publisher (RTSP), tsdemux, ingestor/tsnorm
- **Vị trí:** internal/publisher/dash/demux.go:173-181; internal/publisher/dash/packager.go:305-308; internal/publisher/serve_rtsp.go:386-395; internal/tsdemux/demuxer.go:111-116; internal/ingestor/worker.go:553-559

## Hiện tượng
DASH hoặc RTSP đứng im, trong khi HLS vẫn chạy. Log DASH chỉ có dòng Debug, sau đó lặp cảnh báo `tsBuffer overflow` khi buffer đầy.

## Điều kiện kích hoạt
Dữ liệu TS lỗi đến được publisher. Một đường cụ thể: `tsnorm.Process` lỗi rồi `writeRawTSChunk` rơi về passthrough ("tsnorm demux failed, falling back to passthrough"), chuyển TS gốc sang buffer hub. astits trả lỗi với PES hỏng (`parsePESData`) qua `NextData`.

## Phân tích
`tsdemux.Input` trả về ngay với lỗi khác EOF. DASH chỉ ghi Debug rồi kết thúc goroutine:
```go
if err := dmx.Input(ctx, tb); err != nil && err != io.EOF {
    slog.Debug("dash: demuxer exit", "err", err)
}
```
Trong `Run`, `*tb` khác nil nên demuxer không được tạo lại, tiếp tục ghi vào buffer không ai đọc. RTSP cũng vậy (`startDemux` chỉ chạy một lần, `demuxDone` chỉ đọc khi thoát).

## Vì sao đây là lỗi
Phía ingest đã biết lỗi này: `tsnorm` có `maxDemuxRestarts` và ghi rõ để "a transient corruption ... resyncs instead of permanently killing normalisation". Publisher dùng cùng demuxer mà không có retry.

## Hướng sửa
Đưa vòng restart vào `tsdemux` hoặc dùng chung cho DASH/RTSP: tạo lại `astits.Demuxer` tiếp tục đọc cùng reader, có giới hạn, và log Warn.

## Cách xác minh
Test ghi vào `tb` một PES có header hỏng rồi TS hợp lệ: frame sau lỗi vẫn phải được `OnFrame` nhận.

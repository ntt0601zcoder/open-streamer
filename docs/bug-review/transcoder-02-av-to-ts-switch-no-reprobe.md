# Failover từ input AV-path sang input raw-TS không probe lại định dạng

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder/native, internal/transcoder
- **Vị trí:** internal/transcoder/native/stream_pipeline.go:797-803, internal/transcoder/native/stream_pipeline.go:825-850, internal/transcoder/native/stream_pipeline.go:1018-1025, internal/transcoder/native/stream_pipeline.go:1096-1103, internal/transcoder/supervisor.go:321-347

## Hiện tượng
Stream transcode chính là RTMP/RTSP, dự phòng là UDP/HLS/SRT. Sau failover sang dự phòng, output mất hình và mất tiếng. Khi gặp chunk chứa IDR thì decoder nhận byte TS: CPU decoder có thể lỗi rồi respawn, cuvid có thể ra hình rác.

## Điều kiện kích hoạt
1. Subprocess bắt đầu với input AV-path (RTMP, RTSP, copy, mixer), nên `tsInput == nil` và `formatProbed == true`.
2. Manager chuyển sang input raw-TS.
3. Packet có `SessionStart` kích hoạt `SwitchInput`, rồi các chunk TS tới.

## Phân tích
Việc probe chỉ chạy một lần:

```go
if !p.formatProbed {
    p.formatProbed = true
    if looksLikeTS(data) { ... p.tsInput = newTSInput(p.inputCtx) }
}
```

`SwitchInput` và `switchInputGPU` chỉ reset cờ này trong nhánh có demuxer:

```go
if p.tsInput != nil {
    ...
    p.formatProbed = false
}
```

Với AV-path, `tsInput` là nil nên `formatProbed` vẫn là true. Chunk TS từ supervisor có `codec` là `CODEC_UNSPECIFIED`, đi vào nhánh Annex-B: `ensureVideoDecoder` bỏ qua vì codec unknown, `isVideoKeyframeAnnexB` quét byte TS, và `dec.Decode(data, pts, dts)` nhận nguyên chunk TS với PTS bằng 0. Audio trong TS không được demux nên bị mất hoàn toàn.

## Vì sao đây là lỗi
Comment của `SwitchInput` hứa "next ProcessPacket re-probes", nhưng điều đó chỉ đúng khi input cũ là TS. Chiều TS sang AV thì đúng. Các test switch trong `stream_pipeline_test.go` không có kịch bản AV sang TS.

## Hướng sửa
Đặt `p.formatProbed = false` vô điều kiện trong cả hai đường switch, ngoài khối `if p.tsInput != nil`. Nên gom phần reset chung của hai hàm vào một helper.

## Cách xác minh
Test đơn vị: đưa Annex-B qua `ProcessPacket`, gọi `SwitchInput`, rồi đưa chunk bắt đầu bằng `0x47` và khẳng định `p.tsInput != nil`. Thực tế: cấu hình input ưu tiên RTMP và dự phòng UDP, rồi ngắt RTMP.

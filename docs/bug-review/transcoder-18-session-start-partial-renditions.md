# SessionStart có thể chỉ tới một phần rendition sau khi chuyển input

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder/native, internal/publisher
- **Vị trí:** internal/transcoder/native/stream_pipeline.go:1368-1386, internal/transcoder/native/stream_pipeline.go:1042-1049, internal/publisher/hls.go:311

## Hiện tượng
Sau failover trong chế độ audio passthrough, một số rendition có `EXT-X-DISCONTINUITY` còn rendition khác thì không. Rendition thiếu marker có thể lỗi decode khi audio nguồn đổi sample rate.

## Điều kiện kích hoạt
Audio passthrough, ladder có độ trễ encoder khác nhau giữa các rung (ví dụ `bframes` khác nhau, hoặc encoder khác nhau), và ở lần encode đầu sau switch chỉ một số rung có packet.

## Phân tích
```go
if p.pendingSessionStart && len(out) > 0 {
    p.pendingSessionStart = false
    for j := range out {
        out[j].SessionStart = true
    }
}
```

`out` gom packet của mọi rendition trong một frame. Chỉ cần một rendition có packet là cờ bị tiêu thụ, các rung chưa ra packet sẽ không bao giờ nhận được cờ.

## Vì sao đây là lỗi
Comment ở trên chỉ tính tới trường hợp "zero output", còn trường hợp một phần thì chưa xử lý. `TestStreamPipeline_SwitchInputLatchesSessionStartAndForcedIDR` dùng ladder đồng nhất nên không lộ lỗi. Publisher theo từng rendition chỉ dựa vào cờ này để tạo discontinuity.

## Hướng sửa
Giữ latch theo từng rendition (`pendingSessionStart []bool`) và tiêu thụ khi rendition đó phát packet đầu tiên.

## Cách xác minh
Test với hai rendition có `MaxBFrames` khác nhau: gọi `SwitchInput`, đưa frame mới và kiểm tra mỗi `TargetIndex` đều có đúng một packet mang `SessionStart`.

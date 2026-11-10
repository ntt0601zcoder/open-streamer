# DVR bị bỏ qua không báo lỗi trên stream ABR copy và ABR mixer
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** coordinator, dvr/blob
- **Vị trí:** internal/coordinator/abr_copy.go:12-19, 135-201; internal/coordinator/abr_mixer.go:15-21, 150-247; internal/coordinator/coordinator.go:324-329, 403; internal/domain/copy_shape.go

## Hiện tượng
Stream `copy://X` hoặc `mixer://X,Y` (X có ladder ABR, downstream không có transcoder) được bật `dvr.enabled=true`. API trả 200, nhưng không có recording nào được tạo, timeshift không có dữ liệu, và log không có cảnh báo.

## Điều kiện kích hoạt
Tạo hoặc cập nhật stream ABR copy/mixer với `dvr: {enabled: true}`.

## Phân tích
`startLocked` rẽ sang nhánh riêng trước khi tới `c.startDVR`:
```go
if upstream := c.detectABRCopy(stream); upstream != nil {
    return c.startABRCopy(ctx, stream, upstream)
}
if videoUp, audioUp, ok := c.detectABRMixer(stream); ok {
    return c.startABRMixer(ctx, stream, videoUp, audioUp)
}
```
`startABRCopy` và `startABRMixer` không gọi `startDVR`. `updateLocked` đi full-cycle qua cùng các hàm này nên đổi DVR cũng không có tác dụng. `grep` không thấy kiểm tra DVR trong `copy_shape.go`.

## Vì sao đây là lỗi
Danh sách "v1 limitations" ở đầu `abr_copy.go` và `abr_mixer.go` không nhắc tới DVR, và validator không từ chối cấu hình này. Với `blobProfiles`, stream `mirrored` đã có sẵn thông tin rendition và buffer để ghi. Chủ trương ở chỗ khác là từ chối rõ ràng (raw-TS DVR có cảnh báo, D-2), còn ở đây im lặng hoàn toàn.

## Hướng sửa
Gọi `startDVR(ctx, &mirrored)` trong hai nhánh start và `stopDVR` trong hai nhánh stop. Nếu không hỗ trợ thì từ chối ở validator hoặc ít nhất log Warn và ghi vào tài liệu.

## Cách xác minh
Test coordinator: `SetUpstreamLookupForTesting` trả upstream có ladder, Start stream `copy://` có `DVR.Enabled=true`, kiểm tra dvr spy nhận `StartRecording`. Hiện tại không có lời gọi nào.

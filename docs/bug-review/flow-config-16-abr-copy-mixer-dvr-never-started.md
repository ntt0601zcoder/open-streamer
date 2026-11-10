# DVR không bao giờ chạy trên stream ABR copy và ABR mixer

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/coordinator, internal/dvr/blob, internal/api/handler
- **Vị trí:** internal/coordinator/coordinator.go:324-329, internal/coordinator/coordinator.go:403, internal/coordinator/abr_copy.go:135-201, internal/coordinator/abr_mixer.go, internal/coordinator/coordinator.go:569-575

## Hiện tượng
Stream `copy://X` với X có ABR ladder, hoặc stream ABR mixer, được bật `dvr.enabled: true`. API chấp nhận và stream chạy bình thường nhưng không có bản ghi nào, timeshift trả rỗng. Không có log cảnh báo.

## Điều kiện kích hoạt
1. Stream X có `transcoder.video.profiles` (không `copy`).
2. Tạo stream B với input duy nhất `copy://X`, không có `transcoder`, `dvr: {enabled: true}`.

## Phân tích
`startLocked` rẽ nhánh sớm:

```go
if upstream := c.detectABRCopy(stream); upstream != nil {
    return c.startABRCopy(ctx, stream, upstream)
}
```

`c.startDVR` chỉ được gọi ở cuối đường thường (dòng 403) và trong `reloadDVR`. `startABRCopy` và `startABRMixer` không gọi. `updateLocked` với luồng ABR chỉ stop rồi start nên cũng không qua `reloadDVR`.

Comment của `abr_copy.go` có liệt kê các giới hạn v1 nhưng không có DVR. Rendition buffer của downstream có sẵn, nên `blobProfiles` trên bản `mirrored` có thể dùng được về mặt dữ liệu.

## Vì sao đây là lỗi
Cấu hình được chấp nhận rồi bị bỏ qua hoàn toàn. Đường thường có log cảnh báo khi không ghi được, còn đường này thì im lặng.

## Hướng sửa
Gọi `startDVR` với bản `mirrored` trong `startABRCopy` và `startABRMixer`, và `stopDVR` trong các hàm stop tương ứng. Nếu không hỗ trợ thì từ chối ở `ValidateCopyShape` và `ValidateMixerShape`.

## Cách xác minh
Test coordinator với spy DVR: start stream ABR copy có DVR bật, kiểm tra `StartRecording` được gọi.

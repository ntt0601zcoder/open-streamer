# Các trường interlace, deviceid, language, decoder.name được nhận nhưng không dùng

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/domain, internal/coordinator, internal/transcoder, internal/transcoder/native
- **Vị trí:** internal/domain/transcoder.go:183, internal/domain/transcoder.go:198, internal/domain/transcoder.go:209, internal/domain/transcoder.go:232, internal/coordinator/diff.go:122, internal/transcoder/supervisor.go:654-666, internal/transcoder/native/server.go:425-426, internal/transcoder/native/cuda.go:43, docs/CONFIG.md:281, docs/CONFIG.md:290, docs/CONFIG.md:294, docs/FEATURES_CHECKLIST.md:209

## Hiện tượng
Nguồn interlaced đặt `video.interlace: auto` vẫn ra hình răng cưa. Máy nhiều GPU đặt `global.deviceid: 1` vẫn encode trên GPU 0. `audio.language` không xuất hiện trong metadata. `decoder.name` không có tác dụng. Mỗi lần đổi các trường này còn gây restart subprocess vô ích.

## Điều kiện kích hoạt
`POST /streams/{code}` với `{"transcoder":{"video":{"interlace":"tff"}}}` trên stream đang transcode.

## Phân tích
Tìm trong `internal/transcoder` (kể cả `native` và `transcoder.proto`) không có tham chiếu nào tới `Interlace`, `yadif`, `DeviceID` hay `Language`. `audioConfigFromTC` không gửi language:

```go
return &pb.AudioConfig{
    Copy: tc.Audio.Copy, Codec: string(tc.Audio.Codec),
    ...
    Normalize: tc.Audio.Normalize, Volume: tc.Audio.Volume,
}
```

`tc.Decoder` cũng không được đọc, decoder do `decoderCodecForBackend` chọn. CUDA device luôn tạo với device rỗng (`CreateHardwareDeviceContext(t, "", nil, 0)`). Trong khi đó `reflect.DeepEqual` ở `diffTranscoder` coi đó là thay đổi và kích hoạt `reloadTranscoderFull`.

## Vì sao đây là lỗi
docs/CONFIG.md mô tả các trường này, và docs/FEATURES_CHECKLIST.md ghi "Deinterlace | Complete | yadif (CPU) / yadif_cuda (GPU)". Trường bị rơi mất giữa domain và proto, là một lỗ hổng do migrate sang native transcoder.

## Hướng sửa
Truyền và cài đặt các trường này qua proto, ưu tiên `interlace` và `deviceid`. Nếu chưa làm, trả 400 khi được đặt giá trị và sửa tài liệu.

## Cách xác minh
Đọc `transcoder.proto` để xác nhận không có trường tương ứng. Thử thực tế với nguồn interlaced trên một stream thử.

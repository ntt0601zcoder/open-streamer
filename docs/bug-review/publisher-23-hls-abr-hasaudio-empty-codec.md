# HLS master bỏ audio khi `audio.codec` để trống dù transcoder vẫn encode AAC
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (HLS ABR), transcoder
- **Vị trí:** internal/publisher/hls_abr.go:209-214, 169-172; internal/transcoder/native/stream_pipeline.go:412-422; internal/transcoder/native/server.go:278-280

## Hiện tượng
Stream ABR có audio trong segment nhưng phát trên trình duyệt qua hls.js không có tiếng. Comment trong `hlsCodecString` mô tả đúng triệu chứng này.

## Điều kiện kích hoạt
Có transcoder với `audio.copy=false` và `audio.codec` rỗng, tức cấu hình chỉ khai `video.profiles`.

## Phân tích
```go
hasAudio := stream.Transcoder.Audio.Copy || stream.Transcoder.Audio.Codec != ""
```
Với giá trị zero, `hasAudio=false`, CODECS chỉ có video. Trong khi subprocess quyết định chỉ theo `Copy`:
```go
if cfg.Copy { return nil, nil }
ar, err := newAudioReencoder(cfg, "aac")
```
Nghĩa là `Copy=false` luôn re-encode ra AAC, kể cả khi `Codec` rỗng.

## Vì sao đây là lỗi
Suy luận ở publisher không khớp hành vi transcoder. `AudioCodecAAC` được ghi là mặc định cho HLS/DASH. Không có guard nào đặt Codec mặc định trước khi `hasAudio` được tính.

## Hướng sửa
Xác định `hasAudio` từ thực tế: track audio quan sát được trong segment, hoặc tạm coi `true` khi có transcoder và cập nhật khi biết chắc.

## Cách xác minh
Unit test `serveHLSAdaptive` với `Audio{}` phải tạo `CODECS` có `mp4a.40.2`. Thực tế: tạo stream chỉ có `video.profiles`, kiểm tra master.

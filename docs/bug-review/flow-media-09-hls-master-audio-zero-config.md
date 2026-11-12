# Master playlist bỏ codec audio khi cấu hình audio để trống
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (HLS ABR), transcoder (supervisor, native), domain
- **Vị trí:** internal/publisher/hls_abr.go:214; internal/publisher/hls.go:860-865; internal/transcoder/supervisor.go:653-665; internal/transcoder/native/server.go:360-375; internal/transcoder/native/stream_pipeline.go:416-425; internal/transcoder/native/audio_reencode.go:111-114

## Hiện tượng
Stream ABR có `transcoder.audio` để trống: segment TS vẫn có AAC, nhưng `CODECS` trong master playlist chỉ có video. hls.js có thể tạo SourceBuffer chỉ cho video và không phát tiếng trong trình duyệt. Comment trong `hlsRunOpts` mô tả đúng triệu chứng này.

## Điều kiện kích hoạt
`transcoder.video.profiles` có giá trị, `transcoder.audio` là zero value (`copy=false`, `codec=""`).

## Phân tích
Publisher suy luận:
```go
hasAudio := stream.Transcoder.Audio.Copy || stream.Transcoder.Audio.Codec != ""
```
Trường hợp trống cho kết quả false. Trong khi đó transcoder coi `Copy=false` là re-encode:
```go
if cfg.Copy { return nil, nil }
ar, err := newAudioReencoder(cfg, "aac")
```
và `audio_reencode.go` chọn `aac` khi codec rỗng:
```go
if encName == "" || encName == "aac" { encName = "aac" }
```
Giá trị mặc định `aac` ở `config_defaults.go` chỉ phục vụ API hiển thị, không ghi vào stream.

## Vì sao đây là lỗi
Hai module suy ra hai kết luận khác nhau từ cùng một cấu hình. `CODEC.md` cũng nói codec trống rơi về `aac`.

## Hướng sửa
Đặt `hasAudio` theo dữ liệu thực tế (đã thấy audio trên buffer), hoặc tối thiểu coi `Codec == ""` là có audio. Nguồn không có audio cần được tính riêng.

## Cách xác minh
Test: `writeHLSMasterPlaylist` qua `serveHLSAdaptive` với `AudioTranscodeConfig{}`, kiểm tra `CODECS` có `mp4a.40.2`.

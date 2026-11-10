# Audio codec không qua ResolveAudioEncoder: mp2a/mp3 crash-loop, ac3/eac3 bị gói thành ADTS

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder, internal/transcoder/native, internal/domain
- **Vị trí:** internal/transcoder/supervisor.go:659, internal/transcoder/native/audio_reencode.go:111-118, internal/transcoder/native/audio_reencode.go:403-410, internal/domain/transcoder.go:39-44, internal/domain/resolvers.go:78-93

## Hiện tượng
- `audio.codec: mp2a` hoặc `mp3`: subprocess lỗi lúc khởi tạo và respawn liên tục.
- `audio.codec: ac3` hoặc `eac3`: stream chạy nhưng tiếng không phát được, vì dữ liệu được đánh dấu là AAC.

## Điều kiện kích hoạt
`transcoder.audio.copy=false` với một codec khác `aac` nằm trong danh sách `domain.AudioCodec`.

## Phân tích
Supervisor gửi nguyên giá trị người dùng: `Codec: string(tc.Audio.Codec)`. Reencoder dùng thẳng làm tên encoder:

```go
encName := cfg.Codec
if encName == "" || encName == "aac" { encName = "aac" }
encCodec := astiav.FindEncoderByName(encName)
```

`mp2a` không phải tên encoder libav (đúng là `mp2`), `mp3` cũng vậy (đúng là `libmp3lame`). Bảng ánh xạ đúng nằm ở `domain.ResolveAudioEncoder`, nhưng hàm này không có caller nào ngoài test. Với `ac3`/`eac3` encoder mở được, nhưng `encodeFrame` luôn làm:

```go
adts := wrapADTS(a.encPkt.Data(), a.outRate, a.outChans)
... Codec: esCodecAAC
```

Nên bitstream AC-3 bị thêm header ADTS và đi tiếp dưới nhãn AAC. Ngay cả sau khi sửa tên, `mp2` cũng cần sample format khác `FLTP` và không được gói ADTS.

## Vì sao đây là lỗi
Các giá trị này được khai báo là codec đầu ra hỗ trợ, nhưng đường encode chỉ đúng với AAC. `proto.Codec` cũng chỉ có H264, H265 và AAC nên không thể mô tả audio khác.

## Hướng sửa
Hoặc từ chối mọi codec khác AAC ở validate cho tới khi được hỗ trợ thật, hoặc dùng `ResolveAudioEncoder`, chọn sample format theo encoder, bỏ ADTS với codec khác AAC, và mở rộng `proto.Codec` cùng ánh xạ ở supervisor.

## Cách xác minh
Gọi `newAudioReencoder(AudioConfig{Codec: "mp2a"})` để thấy lỗi "not in linked libavcodec". Với `ac3`, kiểm tra 2 byte đầu output là `0xFF 0xF1` và ffprobe segment HLS.

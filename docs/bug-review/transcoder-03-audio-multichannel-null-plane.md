# audio.channels lớn hơn 2 làm subprocess SIGSEGV ở AudioFifo

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/transcoder/native, internal/domain
- **Vị trí:** internal/transcoder/native/audio_reencode.go:102-109, internal/transcoder/native/audio_reencode.go:141, internal/transcoder/native/audio_reencode.go:163, internal/transcoder/native/audio_reencode.go:282-288, internal/transcoder/native/audio_reencode.go:354-358, internal/domain/transcoder.go:134-135

## Hiện tượng
Đặt `audio.channels: 6` (tài liệu ghi `6 = 5.1 surround`) khiến subprocess chết ngay khi có frame audio đầu tiên, rồi respawn lặp lại.

## Điều kiện kích hoạt
`transcoder.audio.copy=false` và `channels` lớn hơn 2 (ví dụ 6). Không tìm thấy validate nào chặn giá trị này ở domain hay API.

## Phân tích
Layout chỉ có mono hoặc stereo, nhưng FIFO dùng đúng số kênh cấu hình:

```go
outChans := cfg.Channels
layout := astiav.ChannelLayoutStereo
if outChans == 1 { layout = astiav.ChannelLayoutMono }
...
fifo := astiav.AllocAudioFifo(a.outFmt, outChans, frameSize)
```

Với FLTP, FIFO 6 kênh có 6 plane. `resFrame` theo layout stereo nên chỉ có `data[0]` và `data[1]`. `AudioFifo.Write` của go-astiav truyền thẳng `&f.c.data[0]` vào `av_audio_fifo_write`, hàm này đọc từ cả 6 con trỏ plane, trong đó `data[2..5]` là NULL. Chiều `fifo.Read(a.encFrame)` cũng có vấn đề tương tự. Ngoài ra `wrapADTS` sẽ ghi channel config 6 trong khi encoder thực tế là stereo.

## Vì sao đây là lỗi
Encoder mở thành công với stereo nên `NewStreamPipeline` không báo lỗi. Crash ở C không thể recover trong Go, và cấu hình không đổi nên respawn lặp lại.

## Hướng sửa
Xây layout đúng theo số kênh (5.1 cho 6 kênh, và kiểm tra encoder hỗ trợ), hoặc từ chối các giá trị không hỗ trợ ngay ở validate API. Dùng `layout.Channels()` làm nguồn duy nhất cho FIFO và ADTS.

## Cách xác minh
Trong builder image, tạo `newAudioReencoder(AudioConfig{Channels: 6})` rồi gọi `Process` với một frame AAC stereo: tiến trình sẽ SIGSEGV. Mở rộng `audio_resample_test.go` cho trường hợp này sau khi sửa.

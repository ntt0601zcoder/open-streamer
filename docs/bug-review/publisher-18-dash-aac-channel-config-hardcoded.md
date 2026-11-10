# DASH luôn khai AAC stereo, sai với nguồn mono hoặc 5.1
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher/dash, mp4ff
- **Vị trí:** internal/publisher/dash/fmp4_writer.go:93-112; mp4ff v0.52.0 mp4/initsegment.go:278-287

## Hiện tượng
Stream AAC mono hoặc 5.1 phát DASH bị lỗi giải mã, mất tiếng, hoặc sai layout kênh. HLS và RTMP cùng stream phát bình thường vì giữ cấu hình từ ADTS.

## Điều kiện kích hoạt
Nguồn AAC khác 2 kênh, hoặc transcoder cấu hình `audio.channels: 1` hay `6`.

## Phân tích
`BuildAACInit` chỉ lấy sample rate từ header ADTS:
```go
trak.SetAACDescriptor(aac.AAClc, sr)
```
Trong mp4ff, `SetAACDescriptor` tạo `AudioSpecificConfig` với `ChannelConfiguration: 2` cố định. Thông tin `channel_configuration` và profile trong ADTS bị bỏ. Codec string cũng luôn là `mp4a.40.2`.

## Vì sao đây là lỗi
Init là nơi duy nhất decoder lấy cấu hình kênh trong fMP4. Không có guard nào kiểm tra số kênh, và domain cho phép chọn 1, 2, 6 kênh.

## Hướng sửa
Tạo `AudioSpecificConfig` từ profile, sample rate index và channel config của ADTS, rồi dựng `esds` bằng chính cấu hình đó thay vì `SetAACDescriptor`. Có thể khai thêm `AudioChannelConfiguration` trong MPD.

## Cách xác minh
Unit test: header ADTS `channel_configuration=1` phải cho init có ASC một kênh. Thực tế: ffprobe `init_a.mp4` với nguồn mono.

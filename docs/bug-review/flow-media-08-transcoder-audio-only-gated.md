# Nguồn chỉ có audio đi qua transcoder không tạo ra output
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** transcoder (native), publisher, ingestor
- **Vị trí:** internal/transcoder/native/stream_pipeline.go:439-443, 838-844, 879-885; internal/transcoder/native/server.go:172-173; internal/publisher/dash/segmenter.go:143-144, 327-342

## Hiện tượng
Kênh radio AAC có transcoder (ví dụ dùng chung template, hoặc để re-encode bitrate): input báo Active, nhưng HLS và DASH trống hoàn toàn, không có lỗi. Telemetry `DroppedAudioPreKeyframe` tăng liên tục.

## Điều kiện kích hoạt
Nguồn AAC không có video (UDP/HLS/RTMP...), stream có `transcoder` với `shouldRunTranscoder` true.

## Phân tích
Mọi audio phải chờ video IDR:
```go
if !p.sawKeyframe {
    p.stats.DroppedAudioPreKeyframe += uint64(len(frames))
    return nil, nil
}
```
`sawKeyframe` chỉ được bật ở hai nhánh video. Không có video thì cờ không bao giờ bật, nên toàn bộ audio bị bỏ. Không có timeout, log Warn hay terminal error.

## Vì sao đây là lỗi
Publisher có hỗ trợ audio-only rõ ràng (`cutAudioOnly`, test `hls_audio_only_cut_test`), Normaliser cũng có timeout cho single-track. Cổng trong pipeline được thêm để tránh lệch offset sau switch, nhưng không có lối thoát cho stream không có video.

## Hướng sửa
Thêm timeout: nếu sau một khoảng (ví dụ 2-3 giây) vẫn chưa có frame video nào của input hiện tại thì mở cổng cho audio. Hoặc coi stream là audio-only và bỏ qua encoder video.

## Cách xác minh
Test pipeline trong builder image: `ProcessPacket` chỉ với AAC (AV-path) nhiều giây, kiểm tra có `OutputFrame` audio. Hiện tại trả rỗng và counter tăng.

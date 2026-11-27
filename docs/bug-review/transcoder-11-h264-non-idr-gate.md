# Cổng keyframe chỉ nhận IDR nên nguồn H.264 open-GOP có thể không bao giờ ra hình

- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** internal/transcoder/native
- **Vị trí:** internal/transcoder/native/ts_input.go:215-218, internal/transcoder/native/stream_pipeline.go:440-443, internal/transcoder/native/stream_pipeline.go:775-780, internal/transcoder/native/stream_pipeline.go:838-844, internal/transcoder/native/stream_pipeline.go:879-885, internal/transcoder/native/stream_pipeline.go:939-965

## Hiện tượng
Nguồn H.264 chỉ có IDR ở đầu rồi dùng I-frame kèm recovery point (open-GOP, thường gặp ở encoder broadcast). Nếu transcoder vào giữa stream, output trống hoàn toàn cả hình lẫn tiếng, không có lỗi và stream vẫn Active. Chỉ có `dropped_frames` tăng.

## Điều kiện kích hoạt
Subprocess bắt đầu (hoặc `SwitchInput`) sau IDR đầu tiên của một nguồn H.264 không phát IDR định kỳ. Respawn và reconnect đều tạo ra tình huống này.

## Phân tích
Chỉ NAL loại 5 mở được cổng: `gocodec.IsH264IDRFrame` trên đường TS và `isH264KeyframeAnnexB` trên đường AV. Cho tới lúc đó video bị bỏ:

```go
if !p.sawKeyframe {
    if !f.keyframe {
        p.stats.DroppedVideoPreKeyframe++
        continue
    }
```

Audio cũng bị chặn cùng cổng trong `handleAudio`:

```go
if !p.sawKeyframe {
    p.stats.DroppedAudioPreKeyframe += uint64(len(frames))
    return nil, nil
}
```

## Vì sao đây là lỗi
Phía HEVC đã chấp nhận mọi IRAP (`IsH265IDRFrame` nhận NAL 16-21, gồm CRA), còn H.264 không có điểm vào tương đương. libavcodec có thể bắt đầu decode từ recovery point khi có SPS/PPS, nên cổng này chặt hơn khả năng của decoder. Không có timeout để mở cổng, và supervisor không coi việc không có output là unhealthy.

## Hướng sửa
Mở cổng cả khi gặp access unit có SPS, PPS và I-slice kèm recovery point SEI. Hoặc thêm timeout sau N giây thì thử từ I-frame gần nhất, kèm metric hoặc cảnh báo khi cổng đóng quá lâu.

## Cách xác minh
Tạo nguồn bằng `x264 --open-gop` với IDR chỉ ở đầu, bắt đầu feed từ I-frame không phải IDR vào `ProcessPacket`, kiểm tra không có output và `DroppedVideoPreKeyframe` tăng mãi.

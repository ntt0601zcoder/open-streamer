# tfdt DASH nối tiếp không bao giờ neo lại sau gián đoạn, timeline tụt khỏi đồng hồ và lệch V/A khi track quay lại
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher/dash
- **Vị trí:** internal/publisher/dash/packager.go:605-616, 628-635, 918-941, 1038-1054

## Hiện tượng
- Sau mỗi lần mất nguồn/failover, độ trễ DASH tăng dần và không tự giảm. Khi độ tụt vượt `timeShiftBufferDepth`, player kiểm tra availability theo đồng hồ không còn segment hợp lệ để phát.
- Một track chết rồi sống lại: track đó bị lệch so với track kia đúng bằng thời gian đã mất, lip-sync sai vĩnh viễn.

## Điều kiện kích hoạt
1. Nguồn gián đoạn N giây rồi có SessionStart mới: timeline tụt N giây cộng phần queue bị bỏ (tối đa khoảng một segment).
2. Video chết đủ `trackLossTimeout`, `cutAudioOnly` tiếp tục, video quay lại sau N giây: video chậm hơn audio N giây.

## Phân tích
Mọi segment sau segment đầu nối tiếp segment trước:
```go
if n := len(entries); n > 0 {
    last := entries[n-1]
    return last.StartTicks + last.DurTicks
}
return wallclockTicks(now, ast, ...)
```
`onSessionBoundary` không chạm `vSegEntries`/`aSegEntries`. Mỗi track tính riêng, nên track đã im không tăng tfdt. Cổng `behindPrevSegEnd` chỉ chặn chạy nhanh hơn đồng hồ, không kéo timeline tiến lên.

## Vì sao đây là lỗi
Comment `liveTrackPresence` khẳng định "tfdt stays wallclock-anchored so the resumed segment lands at the right live-edge position", và ARCHITECTURE cũng ghi tfdt neo theo wallclock. Từ khi chuyển sang tfdt nối tiếp, điều này không còn đúng.

## Hướng sửa
Khi có session boundary hoặc track quay lại sau khi bị coi là chết, đặt tfdt theo `wallclockTicks` của lần cắt kế tiếp, nhảy tiến nếu chênh quá một ngưỡng nhỏ. Dùng chung mốc cho V và A.

## Cách xác minh
Unit test: phát vài segment, tạo gián đoạn 30 giây rồi có boundary, `S@t` phải gần `(now-AST)`. Test track loss: audio-only rồi video quay lại, tfdt video phải khớp audio.

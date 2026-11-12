# Packet SessionStart bị hub drop khiến transcoder bỏ lỡ SwitchInput

- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** internal/transcoder, internal/buffer
- **Vị trí:** internal/transcoder/supervisor.go:315-320, internal/transcoder/supervisor.go:342-343, internal/buffer/service.go:63-79, internal/transcoder/service.go:371-379

## Hiện tượng
Sau một lần chuyển input khi transcoder đang chậm, decoder và demuxer cũ tiếp tục nhận dữ liệu nguồn mới mà không rebase PTS. Có thể thấy lệch timeline, hoặc mất hình nếu định dạng nguồn đổi giữa TS và AV.

## Điều kiện kích hoạt
Kênh subscriber của transcoder (mặc định 1024 packet) đầy đúng lúc packet đầu của session mới được ghi. Ví dụ NVENC quá tải hoặc burst HLS, kèm input flap.

## Phân tích
`SessionStart` là latch một lần, được gắn vào đúng một packet và fan-out không chặn:

```go
pc := clonePacket(pkt)
select {
case s.ch <- pc:
default:
    // dropped
}
```

Supervisor chỉ gửi Switch khi thấy cờ này:

```go
if pkt.SessionStart && *firstWritten { ... sendSwitch ... }
```

Packet sau có `SessionID` mới nhưng `SessionStart=false`. `InputPacket.session_id` được gửi đi nhưng không ai dùng để so sánh.

## Vì sao đây là lỗi
`Service.NotifyInputSwitch` không có caller nào, nên đường marker là tín hiệu duy nhất. Drop là hành vi đã định của hub, vì vậy consumer không được phụ thuộc vào một packet riêng lẻ. Khả năng xảy ra thấp vì kênh thường rỗng lúc failover, do đó độ tin cậy về tần suất chỉ ở mức trung bình.

## Hướng sửa
Trong `inputPump`, lưu `lastSessionID` và gửi Switch khi `pkt.SessionID` đổi khác giá trị đã thấy (bỏ qua giá trị 0). Giữ `SessionStart` như tín hiệu phụ.

## Cách xác minh
Test đơn vị cho `forwardOnePacket`: đưa packet `SessionID=1`, rồi `SessionID=2` với `SessionStart=false`, khẳng định có gửi Switch.

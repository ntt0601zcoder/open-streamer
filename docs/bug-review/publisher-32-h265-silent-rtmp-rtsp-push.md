# H.265 bị bỏ âm thầm ở RTMP push, RTMP play và RTSP trong khi trạng thái báo bình thường
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (RTMP push, RTMP play, RTSP)
- **Vị trí:** internal/publisher/push_rtmp.go:253-258, 347-354; internal/publisher/push_codec.go:201; internal/publisher/serve_rtmp.go:270-277, 301-304; internal/publisher/serve_rtsp.go:610-615, 625-631

## Hiện tượng
Với stream H.265:
- RTMP push: API báo `active`, đích chỉ nhận audio (metadata khai AVC), không có hình.
- RTMP play: chấp nhận client rồi im lặng, không có cả audio.
- RTSP: `DESCRIBE` trả 404 dù stream đang chạy và không có log giải thích.

## Điều kiện kích hoạt
Nguồn H.265 không transcode, hoặc rung cao nhất dùng `h265`, cùng với các output trên.

## Phân tích
Các nhánh `switch` chỉ xử lý H.264 và AAC, `default` bỏ qua. Push đặt `PushStatusActive` ngay sau handshake, không phụ thuộc có video gửi được hay không. RTMP play và RTSP giữ audio cho đến keyframe đầu (`firstVideo`), nhưng H.265 không đi vào nhánh video nên cổng này không mở.

## Vì sao đây là lỗi
Comment nêu việc bỏ là chủ ý, nhưng chỉ ở mức code. API không báo, không có cảnh báo trong log, và validate không từ chối cấu hình này. Người vận hành nhìn trạng thái `active` sẽ nghĩ output vẫn tốt.

## Hướng sửa
Khi thấy frame H.265 ở các output này: ghi Warn một lần, báo lỗi vào trạng thái push (hoặc chuyển `failed`), từ chối RTMP play và trả lỗi RTSP rõ ràng. Hoặc hỗ trợ Enhanced RTMP và RTP H.265.

## Cách xác minh
Test feed H.265 qua `onTSFrame`: push phải có lỗi/trạng thái quan sát được. Thực tế: ingest HEVC rồi xem `runtime.publisher.pushes[]`.

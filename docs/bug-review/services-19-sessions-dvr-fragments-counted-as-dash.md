# Một viewer timeshift bị tính thành hai session, phần lớn byte gắn sai giao thức và sai cờ DVR

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/sessions, internal/dvr/blob
- **Vị trí:** internal/sessions/httpmw.go:51-54, internal/sessions/httpmw.go:112-128, internal/dvr/blob/hls.go:82, internal/dvr/blob/hls.go:105, internal/sessions/fingerprint.go:31-49

## Hiện tượng
Một người xem HLS timeshift tạo hai bản ghi: một `hls, dvr=true` chỉ có byte playlist, và một `dash, dvr=false` chứa toàn bộ byte media. Số viewer và phân bố giao thức, live/DVR đều sai.

## Điều kiện kích hoạt
Phát `/<code>/index.m3u8?from=...` từ archive CMAF. Với DASH timeshift cũng tách đôi vì fragment không có query DVR.

## Phân tích
Giao thức chỉ dựa vào đuôi file, và cờ DVR chỉ dựa vào query:
```go
case ".mpd", ".m4s", ".mp4":
    return domain.SessionProtoDASH
```
```go
return q.Has("from") || q.Has("delay") || q.Has("dur") || q.Has("ago")
```
URI do `RenderMediaPlaylist` sinh ra là `dvri-*.mp4`, `dvrf-*.m4s` và không có query. Protocol và DVR nằm trong fingerprint, nên chúng rơi vào session khác với playlist.

## Vì sao đây là lỗi
Comment `fingerprintID` nói mục đích tham gia key là để giữ nhãn Protocol/DVR đúng. Tracker cũng có `blob`-agnostic path nhưng tên file `dvr*` cho biết rõ đây là DVR.

## Hướng sửa
Trong middleware, coi tên file bắt đầu bằng `dvri-`, `dvrf-`, `dvrt-` là DVR. Với giao thức: `dvrt-` là DASH; `dvrf-` là HLS; `dvri-` cần thêm thông tin, ví dụ thêm hậu tố khác nhau theo renderer.

## Cách xác minh
Test middleware: `GET /a/index.m3u8?from=1` rồi `GET /a/dvrf-p0-2026010100-0-0.m4s` với cùng IP/UA, kiểm tra hiện có hai session.

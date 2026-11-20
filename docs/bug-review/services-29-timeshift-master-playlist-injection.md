# Tham số timeshift không được validate, chèn được dòng tuỳ ý vào master playlist

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/dvr/blob
- **Vị trí:** internal/api/handler/blob_timeshift.go:106-109, internal/api/handler/blob_timeshift.go:238-246, internal/dvr/blob/hls.go:20-21, internal/dvr/blob/hls.go:43

## Hiện tượng
Link được tạo sẵn khiến master playlist hợp lệ của server trỏ player tới variant trên host của kẻ tấn công. Đầu vào sai như `?from=abc` cũng nhận 200 thay vì 400.

## Điều kiện kích hoạt
Stream có archive CMAF. Mở:
`/<code>/index.m3u8?from=1%0Ahttps://evil.example/x.m3u8%0A%23`

Với stream có policy, kẻ tấn công cần một token hợp lệ hoặc nạn nhân tự có token.

## Phân tích
Nhánh master trả về trước mọi bước parse:
```go
if q.Get("profile") == "" {
    _, _ = w.Write([]byte(blob.RenderMaster(cat, timeshiftParams(q))))
    return
}
```
`timeshiftParams` ghép giá trị đã giải mã, gồm cả xuống dòng:
```go
parts = append(parts, k+"="+v)
```
`RenderMaster` in thẳng vào playlist (`index.m3u8?%s&profile=%s&track=v\n`), nên `%0A` tách thành dòng URI mới.

## Vì sao đây là lỗi
Các nhánh còn lại đã validate bằng `parseTimeshiftStart`/`parseTimeshiftDuration`, riêng nhánh master thì không. Nội dung phản chiếu từ query thành URI mà player sẽ tải.

## Hướng sửa
Parse và validate `from/delay/ago/dur` ở đầu `ServeTimeshift`, sau đó dựng lại bằng `url.Values.Encode()`.

## Cách xác minh
Unit test handler: query có `%0A`, kiểm tra body hiện chứa dòng `https://evil.example/x.m3u8`.

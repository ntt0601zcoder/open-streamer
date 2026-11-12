# Hot-reload GeoIP giải phóng mmap khi lookup khác đang đọc, có thể làm chết process

- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** internal/sessions, internal/mediaauth, maxminddb
- **Vị trí:** internal/sessions/geoip.go:31-36, internal/sessions/geoip.go:68-74, internal/sessions/geoip.go:82-98, internal/sessions/geoip_maxmind.go:53-63, internal/sessions/geoip_maxmind.go:68-75, internal/sessions/service.go:118-127, internal/mediaauth/mediaauth.go:168-170, internal/sessions/tracker.go:346

## Hiện tượng
Đổi `sessions.geoip_db_path` khi đang có lưu lượng có thể làm server crash với `unexpected fault address` hoặc nil pointer, kéo theo mọi stream.

## Điều kiện kích hoạt
Đang có viewer mới, hoặc policy có luật quốc gia (lookup trên mỗi request media), rồi `POST /config` đổi `geoip_db_path`. Lookup đang chạy đúng lúc `Set` đóng reader cũ.

## Phân tích
```go
old := s.current.Swap(&geoipHolder{r: r, c: c})
if old != nil && old.c != nil {
    _ = old.c.Close()
}
```
`Country` đã `Load` holder cũ có thể vẫn đang trong `db.Country(ip)`. `Reader.Close` của maxminddb gọi `munmap(r.buffer)`, nên đọc tiếp là truy cập vùng nhớ đã unmap. Lỗi fault trên địa chỉ không phải nil không `recover` được. Ngoài ra `MaxMindGeoIP.Close` gán `m.db = nil` không đồng bộ, tạo data race với bước kiểm tra `m.db == nil` rồi gọi `m.db.Country`.

## Vì sao đây là lỗi
Comment type khẳng định "in-flight lookups against the previous resolver complete normally because Set never closes the old resolver until after the new one is published". Publish trước rồi close không đảm bảo lookup đã load xong. Cửa sổ lỗi ngắn, nên đánh giá độ tin cậy ở mức xảy ra thực tế là trung bình, còn race thì chắc chắn tồn tại.

## Hướng sửa
Đếm tham chiếu holder (acquire/release quanh lookup) và chỉ close khi về 0, hoặc close trễ sau một khoảng grace an toàn, hoặc dùng `RWMutex`. Bỏ gán `m.db = nil`.

## Cách xác minh
Test stress với `-race`: nhiều goroutine gọi `Country` trên reader mmdb thật, song song `Set` luân phiên hai reader. Chỉ chạy ở môi trường riêng.

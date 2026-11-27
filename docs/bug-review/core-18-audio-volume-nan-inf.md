# ParseAudioVolume chấp nhận NaN và Inf, audio đưa vào encoder bị hỏng
- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** domain, transcoder
- **Vị trí:** internal/domain/transcoder.go:156-176; internal/transcoder/native/audio_reencode.go:134-138,318-338

## Hiện tượng
Đặt `audio.volume` là `NaN`, `Inf` hoặc `NaNdB`: API chấp nhận và không báo lỗi. Với `NaN`, audio ra lỗi encoder hoặc im lặng. Với `Inf`, mọi mẫu khác 0 bị kẹp ở mức tối đa.

## Điều kiện kích hoạt
PUT stream có `transcoder.audio.volume` là một trong các giá trị trên và audio được re-encode.

## Phân tích
`strconv.ParseFloat` chấp nhận `NaN`, `Inf`. Kiểm tra `f < 0` sai với NaN, nên hàm trả về NaN không lỗi. Trong `applyGain`, `s > 1` và `s < -1` đều sai với NaN, nên NaN được ghi vào frame. Nhánh dB không có kiểm tra nào.

Grep không thấy handler API nào gọi `ParseAudioVolume`, nên giá trị chỉ được kiểm ở subprocess.

## Vì sao đây là lỗi
Hàm có mục đích từ chối giá trị không hợp lệ, và clamp có mục đích giữ mẫu trong dải.

## Hướng sửa
Từ chối bằng `math.IsNaN`/`math.IsInf` ở cả hai nhánh, và gọi `ParseAudioVolume` khi validate API.

## Cách xác minh
Unit test: `ParseAudioVolume("NaN")`, `("Inf")`, `("InfdB")` phải trả về lỗi.

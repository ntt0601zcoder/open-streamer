# POST stream giữ lại trường của phần tử mảng cũ, có thể gửi nhầm credential sang input khác

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, encoding/json, internal/store, internal/ingestor, internal/publisher
- **Vị trí:** internal/api/handler/stream.go:665-679, /usr/local/go/src/encoding/json/decode.go:550-563, internal/domain/input.go:41, internal/domain/input.go:49, internal/domain/input.go:81, internal/api/handler/config.go:213

## Hiện tượng
Client gửi `inputs` hoặc `push` mới với phần tử không ghi rõ `headers`, `params`, `net`, các trường đó vẫn được giữ theo **vị trí** từ phần tử cũ. Khi đảo thứ tự hoặc thay input, `Authorization` hay `passphrase` của nguồn A bị gửi đến nguồn B. Bỏ khoá khỏi `headers` cũng không xoá được khoá đó.

## Điều kiện kích hoạt
Stream hiện có:

```json
"inputs":[{"url":"https://a/x.m3u8","priority":0,"headers":{"Authorization":"Bearer A"}},
          {"url":"https://b/y.m3u8","priority":1}]
```

Gửi:

```json
{"inputs":[{"url":"https://b/y.m3u8","priority":0},{"url":"https://a/x.m3u8","priority":1}]}
```

## Phân tích
`decodeStreamBody` cố ý decode đè lên bản clone của stream hiện tại:

```go
if err := json.NewDecoder(r.Body).Decode(&base); err != nil {
```

Với mảng, `encoding/json` decode thẳng vào phần tử sẵn có và không zero phần tử đó:

```go
if i < v.Len() {
    // Decode into element.
    if err := d.value(v.Index(i)); err != nil {
```

Nên `base.Inputs[0]` giữ `Headers` của A và chỉ đổi `URL`. Map cũng được merge giữ khoá cũ. Bản đã merge được lưu vào store, `inputsEqual` thấy khác URL và ingestor kết nối B với header của A. Hiện tượng tương tự xảy ra với `push[i]` và `transcoder.video.profiles[i]`, ví dụ bỏ `bframes` khỏi profile thì giá trị cũ vẫn còn. `POST /config` dùng cùng cách merge cho `auth.api.users`.

## Vì sao đây là lỗi
Merge cấp trường ở gốc là tính năng đã công bố, nhưng client hợp lý sẽ coi mảng là thay thế toàn bộ. Comment trong hàm chỉ xử lý alias con trỏ và không đề cập việc này. Không có test nào cho việc thay mảng.

## Hướng sửa
Decode body vào `map[string]json.RawMessage` để biết khoá nào có mặt, rồi với mảng và map đặt trường về nil trên `base` trước khi decode. Hoặc decode các trường đó vào biến mới.

## Cách xác minh
Test handler với ví dụ trên: sau khi lưu, input có URL B phải không có `Headers`.

package subscription

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxBodySize ограничивает размер подписки: сотня серверов укладывается
// в десятки килобайт, всё остальное — подозрительно.
const maxBodySize = 4 << 20

// Fetch скачивает подписку по ссылке и возвращает её тело.
func Fetch(ctx context.Context, rawURL string) (string, error) {
	if err := ValidateURL(rawURL); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	// Часть провайдеров отдаёт разный формат в зависимости от клиента;
	// нейтральный агент даёт обычный список ссылок.
	req.Header.Set("User-Agent", "kvas-web")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("подписка недоступна: %s", cleanError(err, rawURL))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("сервер подписки ответил кодом %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return "", fmt.Errorf("не удалось прочитать подписку: %w", err)
	}
	return string(body), nil
}

// ValidateURL проверяет ссылку на подписку до того, как она будет
// сохранена или запрошена.
func ValidateURL(rawURL string) error {
	u := strings.TrimSpace(rawURL)
	switch {
	case u == "":
		return fmt.Errorf("ссылка на подписку не указана")
	case !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://"):
		return fmt.Errorf("ссылка должна начинаться с http:// или https://")
	case len(u) > 2048:
		return fmt.Errorf("ссылка слишком длинная")
	}
	return nil
}

// FetchAndParse — обычная последовательность: скачать и разобрать.
func FetchAndParse(ctx context.Context, rawURL string) ([]Server, error) {
	body, err := Fetch(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	return Parse(body)
}

// MaskURL прячет секретную часть ссылки: в интерфейсе и журнале достаточно
// видеть, что подписка задана, а токен доступа показывать незачем.
func MaskURL(raw string) string {
	if raw == "" {
		return ""
	}
	idx := strings.LastIndex(raw, "/")
	if idx < 0 || idx == len(raw)-1 {
		return raw
	}
	head, tail := raw[:idx+1], raw[idx+1:]
	if len(tail) <= 8 {
		return head + strings.Repeat("•", len(tail))
	}
	return head + tail[:4] + strings.Repeat("•", 6) + tail[len(tail)-4:]
}

// cleanError убирает из текста ошибки полную ссылку: клиент HTTP
// подставляет её в сообщение, а она содержит токен подписки.
func cleanError(err error, rawURL string) string {
	msg := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		msg = urlErr.Err.Error()
	}
	return strings.ReplaceAll(msg, rawURL, MaskURL(rawURL))
}

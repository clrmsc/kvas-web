package autovpn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/config"
	"github.com/clrmsc/kvas-web/web/internal/keenetic"
	"github.com/clrmsc/kvas-web/web/internal/kvas"
	"github.com/clrmsc/kvas-web/web/internal/probe"
	"github.com/clrmsc/kvas-web/web/internal/subscription"
)

// Manager владеет состоянием подписки и умеет проверять серверы
// и переключать на них Квас.
type Manager struct {
	cfg config.Config
	log *slog.Logger

	mu    sync.Mutex
	state State

	// running не даёт запустить две проверки разом: каждая поднимает xray,
	// а на роутере это заметная нагрузка.
	running   bool
	runningMu sync.Mutex
}

// New загружает сохранённое состояние подписки.
func New(cfg config.Config, log *slog.Logger) (*Manager, error) {
	st, err := loadState(cfg.SubscriptionFile())
	if err != nil {
		// Повреждённый файл не должен мешать работе остального интерфейса.
		log.Warn("состояние подписки не прочитано", "err", err)
		st = DefaultState()
	}
	return &Manager{cfg: cfg, log: log, state: st}, nil
}

// xrayBin возвращает путь к xray: заданный флагом или найденный среди
// обычных мест установки.
func (m *Manager) xrayBin() string {
	if m.cfg.XrayBin != "" {
		return m.cfg.XrayBin
	}
	return kvas.FindFile(kvas.XrayBinCandidates...)
}

// xrayInit возвращает init-скрипт xray. Квас держит его у себя и при
// настройке делает ссылку /opt/etc/init.d/S24xray, поэтому путь зависит
// от того, выполнялся ли уже kvas setup.
func (m *Manager) xrayInit() string {
	if m.cfg.XrayInit != "" {
		return m.cfg.XrayInit
	}
	return kvas.FindFile(kvas.XrayInitCandidates...)
}

// State возвращает копию текущего состояния.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Settings — изменяемые пользователем настройки.
type Settings struct {
	URL       *string
	Enabled   *bool
	AutoApply *bool
	CheckTime *string
	SpeedTopN *int
}

// UpdateSettings применяет только переданные поля.
func (m *Manager) UpdateSettings(s Settings) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := m.state
	if s.URL != nil {
		url := *s.URL
		if url != "" {
			if err := subscription.ValidateURL(url); err != nil {
				return m.state, err
			}
		}
		next.URL = url
		// Ссылка сменилась — прежние результаты относятся к другой подписке.
		if url != m.state.URL {
			next.Results = []probe.Result{}
			next.LastCheck = time.Time{}
			next.LastError = ""
		}
	}
	if s.Enabled != nil {
		next.Enabled = *s.Enabled
	}
	if s.AutoApply != nil {
		next.AutoApply = *s.AutoApply
	}
	if s.CheckTime != nil {
		if err := ValidateCheckTime(*s.CheckTime); err != nil {
			return m.state, err
		}
		next.CheckTime = *s.CheckTime
	}
	if s.SpeedTopN != nil {
		n := *s.SpeedTopN
		if n < 1 || n > 20 {
			return m.state, fmt.Errorf("проверять по скорости можно от 1 до 20 серверов")
		}
		next.SpeedTopN = n
	}

	if err := saveState(m.cfg.SubscriptionFile(), next); err != nil {
		return m.state, err
	}
	m.state = next
	return next, nil
}

// Servers скачивает подписку и разбирает её.
func (m *Manager) Servers(ctx context.Context) ([]subscription.Server, error) {
	st := m.State()
	if !st.Configured() {
		return nil, fmt.Errorf("ссылка на подписку не задана")
	}
	return subscription.FetchAndParse(ctx, st.URL)
}

// CheckRun — идущая проверка. События приходят в Events по мере готовности,
// закрытие канала означает, что проверка завершилась.
type CheckRun struct {
	Events <-chan probe.Result
}

// maxCheckDuration ограничивает фоновую проверку: три десятка серверов
// с замерами скорости укладываются в минуты, всё дольше — зависание.
const maxCheckDuration = 30 * time.Minute

// StartCheck запускает проверку в фоне и возвращает поток её событий.
// Проверка живёт независимо от HTTP-запроса: если пользователь закрыл
// вкладку, она всё равно доведётся до конца и переключит туннель.
func (m *Manager) StartCheck() (*CheckRun, error) {
	m.runningMu.Lock()
	defer m.runningMu.Unlock()
	if m.running {
		return nil, ErrCheckInProgress
	}
	m.running = true

	events := make(chan probe.Result, 64)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), maxCheckDuration)
		defer cancel()
		defer close(events)
		defer func() {
			m.runningMu.Lock()
			m.running = false
			m.runningMu.Unlock()
		}()

		m.runCheck(ctx, func(r probe.Result) {
			// Подписчик может читать медленнее или уже отключиться —
			// проверку это тормозить не должно.
			select {
			case events <- r:
			default:
			}
		})
	}()

	return &CheckRun{Events: events}, nil
}

// ErrCheckInProgress возвращается, когда проверка уже идёт.
var ErrCheckInProgress = errors.New("проверка серверов уже идёт")

// Running сообщает, идёт ли проверка прямо сейчас.
func (m *Manager) Running() bool {
	m.runningMu.Lock()
	defer m.runningMu.Unlock()
	return m.running
}

// runCheck выполняет саму проверку: скачивает подписку, меряет серверы,
// сохраняет результаты и при включённом автоприменении переключает туннель.
func (m *Manager) runCheck(ctx context.Context, onResult func(probe.Result)) ([]probe.Result, error) {
	servers, err := m.Servers(ctx)
	if err != nil {
		m.recordError(err)
		return nil, err
	}

	st := m.State()
	opt := probe.DefaultOptions()
	opt.XrayBin = m.xrayBin()
	opt.SpeedTestURL = m.cfg.SpeedTestURL
	if m.cfg.TunnelURL != "" {
		opt.TunnelURL = m.cfg.TunnelURL
	}
	opt.SpeedTopN = st.SpeedTopN
	opt.ActiveKey = st.ActiveKey

	// Прошлые замеры скорости передаём в проверку: они задают, у кого
	// мерить в этот раз, и сохраняются для остальных серверов.
	prev := make(map[string]probe.Result, len(st.Results))
	for _, r := range st.Results {
		prev[r.Key] = r
	}

	m.log.Info("проверка серверов подписки началась", "серверов", len(servers))
	results := probe.CheckAll(ctx, servers, opt, prev, onResult)
	probe.Sort(results)

	m.mu.Lock()
	m.state.Results = results
	m.state.History = updateHistory(m.state.History, results, time.Now())
	m.state.LastCheck = time.Now()
	m.state.LastError = ""
	stateCopy := m.state
	m.mu.Unlock()
	if err := saveState(m.cfg.SubscriptionFile(), stateCopy); err != nil {
		m.log.Warn("состояние подписки не сохранено", "err", err)
	}

	alive := 0
	for _, r := range results {
		if r.Alive() {
			alive++
		}
	}
	m.log.Info("проверка серверов подписки завершена", "доступно", alive, "всего", len(results))

	if !stateCopy.AutoApply || len(results) == 0 || !results[0].Alive() {
		return results, nil
	}

	// Победитель может опираться на замер прошлых суток. Переключаться по
	// устаревшему числу нельзя: перемеряем его и пересортировываем.
	if results[0].SpeedStale {
		results = m.refreshWinner(ctx, servers, results, opt)
		if len(results) == 0 || !results[0].Alive() {
			return results, nil
		}
	}
	if results[0].Key == stateCopy.ActiveKey {
		m.log.Info("лучший сервер уже используется", "сервер", results[0].Name)
		return results, nil
	}
	if keep, reason := keepCurrent(results, stateCopy.ActiveKey); keep {
		m.log.Info("остаёмся на текущем сервере", "сервер", stateCopy.ActiveName, "причина", reason)
		return results, nil
	}
	if err := m.Apply(ctx, results[0].Key); err != nil {
		m.log.Error("не удалось переключиться на лучший сервер",
			"сервер", results[0].Name, "err", err)
		m.recordError(err)
	}
	return results, nil
}

// refreshWinner перемеряет скорость у сервера, стоящего первым, если та
// взята из прошлой проверки, и заново упорядочивает результаты.
func (m *Manager) refreshWinner(ctx context.Context, servers []subscription.Server,
	results []probe.Result, opt probe.Options) []probe.Result {

	winner := results[0]
	var target *subscription.Server
	for i := range servers {
		if servers[i].Key() == winner.Key {
			target = &servers[i]
			break
		}
	}
	if target == nil {
		return results
	}

	speed, err := probe.Speed(ctx, *target, opt)
	for i := range results {
		if results[i].Key != winner.Key {
			continue
		}
		if err != nil {
			results[i].SpeedError = err.Error()
			results[i].Speed = 0
			results[i].SpeedStale = false
			m.log.Info("замер лучшего сервера не удался", "сервер", winner.Name, "err", err)
		} else {
			results[i].Speed = speed
			results[i].SpeedStale = false
			m.log.Info("лучший сервер перемерян", "сервер", winner.Name, "мбит/с", speed)
		}
		break
	}
	probe.Sort(results)

	m.mu.Lock()
	m.state.Results = results
	stateCopy := m.state
	m.mu.Unlock()
	if err := saveState(m.cfg.SubscriptionFile(), stateCopy); err != nil {
		m.log.Warn("состояние подписки не сохранено", "err", err)
	}
	return results
}

// switchGain — насколько лучший сервер должен превосходить текущий, чтобы
// переключение имело смысл. Без запаса туннель дёргался бы каждую ночь
// между серверами, отличающимися на пару процентов.
const switchGain = 1.15

// keepCurrent решает, стоит ли остаться на текущем сервере: если он
// по-прежнему доступен и почти не хуже лучшего, переключение только зря
// рвёт соединения.
func keepCurrent(results []probe.Result, activeKey string) (bool, string) {
	if activeKey == "" {
		return false, ""
	}
	var active *probe.Result
	for i := range results {
		if results[i].Key == activeKey {
			active = &results[i]
			break
		}
	}
	if active == nil || !active.Alive() {
		return false, ""
	}

	best := results[0]
	// Скорость известна у обоих — сравниваем её с запасом.
	if best.Speed > 0 && active.Speed > 0 {
		if best.Speed < active.Speed*switchGain {
			return true, "выигрыш по скорости меньше 15%"
		}
		return false, ""
	}
	// Скорости нет — сравниваем задержку, тоже с запасом.
	if active.Latency > 0 && best.Latency > 0 && best.Latency > active.Latency/switchGain {
		return true, "выигрыш по задержке незначителен"
	}
	return false, ""
}

// Apply переключает Квас на сервер с указанным ключом (адрес:порт).
// Прежний конфиг сохраняется: если xray не поднимется, возвращаем как было.
func (m *Manager) Apply(ctx context.Context, key string) error {
	servers, err := m.Servers(ctx)
	if err != nil {
		return err
	}
	var target *subscription.Server
	for i := range servers {
		if servers[i].Key() == key {
			target = &servers[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("сервер %s не найден в подписке", key)
	}

	opt := subscription.DefaultXrayOptions()
	opt.ListenPort = m.cfg.ProxyPort
	cfgData, err := subscription.XrayConfig(*target, opt)
	if err != nil {
		return err
	}

	// Проверяем конфигурацию до подмены рабочей: xray откажется стартовать
	// с ошибочной, и туннель останется лежать.
	if err := m.testConfig(ctx, cfgData); err != nil {
		return err
	}

	backup, hadBackup, err := m.backupConfig()
	if err != nil {
		return err
	}
	if err := kvas.WriteFileAtomic(m.cfg.XrayConf, cfgData, 0o644); err != nil {
		return fmt.Errorf("не удалось записать конфигурацию xray: %w", err)
	}

	if err := m.restartXray(ctx); err != nil {
		m.rollback(ctx, backup, hadBackup)
		return err
	}
	if err := m.waitProxy(ctx); err != nil {
		m.rollback(ctx, backup, hadBackup)
		return fmt.Errorf("туннель не поднялся на сервере %s: %w", target.Name, err)
	}

	// Прокси-интерфейс Keenetic теряет связь с локальным SOCKS при
	// перезапуске xray и сам не встаёт. Без него в таблице маршрутизации
	// туннеля нет маршрута, и помеченный трафик уходит в blackhole —
	// то есть у части устройств просто пропадает интернет.
	if err := m.raiseInterface(ctx); err != nil {
		m.rollback(ctx, backup, hadBackup)
		return fmt.Errorf("сервер %s применён, но интерфейс туннеля не поднялся: %w",
			target.Name, err)
	}

	m.mu.Lock()
	m.state.ActiveKey = target.Key()
	m.state.ActiveName = target.Name
	m.state.AppliedAt = time.Now()
	stateCopy := m.state
	m.mu.Unlock()
	if err := saveState(m.cfg.SubscriptionFile(), stateCopy); err != nil {
		m.log.Warn("состояние подписки не сохранено", "err", err)
	}

	m.log.Info("туннель переключён на сервер подписки",
		"сервер", target.Name, "адрес", target.Endpoint())
	return nil
}

// testConfig просит xray проверить конфигурацию во временном файле.
func (m *Manager) testConfig(ctx context.Context, cfgData []byte) error {
	tmp, err := os.CreateTemp("", "kvasweb-xray-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(cfgData); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	bin := m.xrayBin()
	if bin == "" {
		return fmt.Errorf("xray не найден. Искали: %s.\nУстановите его: opkg install xray",
			strings.Join(kvas.XrayBinCandidates, ", "))
	}
	testCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(testCtx, bin, "run", "-test", "-c", tmp.Name()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("xray отверг конфигурацию: %s", kvas.StripANSI(string(out)))
	}
	return nil
}

func (m *Manager) backupConfig() (string, bool, error) {
	data, err := os.ReadFile(m.cfg.XrayConf)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	backup := filepath.Join(filepath.Dir(m.cfg.XrayConf), filepath.Base(m.cfg.XrayConf)+".kvasweb-bak")
	if err := kvas.WriteFileAtomic(backup, data, 0o644); err != nil {
		return "", false, err
	}
	return backup, true, nil
}

func (m *Manager) rollback(ctx context.Context, backup string, had bool) {
	if !had {
		return
	}
	data, err := os.ReadFile(backup)
	if err != nil {
		m.log.Error("не удалось прочитать резервную копию конфигурации xray", "err", err)
		return
	}
	if err := kvas.WriteFileAtomic(m.cfg.XrayConf, data, 0o644); err != nil {
		m.log.Error("не удалось вернуть прежнюю конфигурацию xray", "err", err)
		return
	}
	if err := m.restartXray(ctx); err != nil {
		m.log.Error("прежняя конфигурация возвращена, но xray не перезапустился", "err", err)
		return
	}
	m.log.Warn("переключение отменено, возвращён прежний сервер")
}

func (m *Manager) restartXray(ctx context.Context) error {
	init := m.xrayInit()
	if init == "" {
		return fmt.Errorf("не найден init-скрипт xray. Искали: %s.\n"+
			"Обычно он появляется после первичной настройки: kvas setup",
			strings.Join(kvas.XrayInitCandidates, ", "))
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(runCtx, init, "restart").CombinedOutput()
	if err != nil {
		return fmt.Errorf("xray не перезапустился: %s", kvas.StripANSI(string(out)))
	}
	return nil
}

// EnsureTunnel возвращает туннель к жизни, если он лежит: заново пишет
// конфигурацию выбранного сервера и поднимает интерфейс.
//
// Понадобилось после `kvas upgrade`: он удаляет /opt/etc/xray вместе с
// конфигурацией, и туннель молча остаётся выключенным. То же самое
// случается после перезагрузки роутера, если что-то не поднялось.
func (m *Manager) EnsureTunnel(ctx context.Context) (bool, error) {
	st := m.State()
	if st.ActiveKey == "" {
		// Сервер ещё не выбирали — восстанавливать нечего.
		return false, nil
	}
	if m.tunnelHealthy(ctx) {
		return false, nil
	}

	m.log.Warn("туннель не работает, восстанавливаем", "сервер", st.ActiveName)
	if err := m.Apply(ctx, st.ActiveKey); err != nil {
		return false, err
	}
	m.log.Info("туннель восстановлен", "сервер", st.ActiveName)
	return true, nil
}

// tunnelHealthy проверяет три вещи разом: есть ли конфигурация, слушает ли
// xray свой порт и поднят ли прокси-интерфейс роутера.
func (m *Manager) tunnelHealthy(ctx context.Context) bool {
	if _, err := os.Stat(m.cfg.XrayConf); err != nil {
		return false
	}
	if !kvas.PortListening(m.cfg.ProxyPort) {
		return false
	}

	name, err := kvas.Conf{Path: m.cfg.KvasConf}.Get("INFACE_CLI")
	if err != nil || strings.TrimSpace(name) == "" {
		// Прокси-интерфейс не настроен — судим только по порту.
		return true
	}
	st, err := keenetic.New(m.cfg.RCIAddr).Interface(ctx, strings.TrimSpace(name))
	if err != nil {
		// Роутер не ответил — не выдумываем поломку.
		return true
	}
	return st.Up()
}

// WatchTunnel периодически проверяет туннель и поднимает его при падении.
func (m *Manager) WatchTunnel(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := m.EnsureTunnel(ctx); err != nil {
				m.log.Error("не удалось восстановить туннель", "err", err)
			}
		}
	}
}

// RestartTunnel перезапускает xray и поднимает прокси-интерфейс, проверяя,
// что туннель действительно ожил. Используется после замены бинарника.
func (m *Manager) RestartTunnel(ctx context.Context) error {
	if err := m.restartXray(ctx); err != nil {
		return err
	}
	if err := m.waitProxy(ctx); err != nil {
		return err
	}
	return m.raiseInterface(ctx)
}

// XrayBin возвращает путь к используемому клиенту xray.
func (m *Manager) XrayBin() string { return m.xrayBin() }

// raiseInterface поднимает прокси-интерфейс Keenetic, через который Квас
// заворачивает трафик в туннель.
func (m *Manager) raiseInterface(ctx context.Context) error {
	name, err := kvas.Conf{Path: m.cfg.KvasConf}.Get("INFACE_CLI")
	if err != nil || strings.TrimSpace(name) == "" {
		// Интерфейс не настроен — значит Квас работает без прокси-интерфейса
		// Keenetic, и поднимать нечего.
		m.log.Debug("INFACE_CLI не задан, интерфейс не поднимаем")
		return nil
	}
	name = strings.TrimSpace(name)

	rci := keenetic.New(m.cfg.RCIAddr)
	if err := rci.InterfaceUp(ctx, name); err != nil {
		return fmt.Errorf("не удалось отправить команду роутеру: %w", err)
	}
	if err := rci.WaitInterfaceUp(ctx, name, 20*time.Second); err != nil {
		return err
	}
	m.log.Info("интерфейс туннеля поднят", "интерфейс", name)
	return nil
}

// waitProxy убеждается, что xray снова слушает рабочий SOCKS-порт.
func (m *Manager) waitProxy(ctx context.Context) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if kvas.PortListening(m.cfg.ProxyPort) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("порт %d не открылся", m.cfg.ProxyPort)
}

func (m *Manager) recordError(err error) {
	m.mu.Lock()
	m.state.LastError = err.Error()
	m.state.LastCheck = time.Now()
	stateCopy := m.state
	m.mu.Unlock()
	if saveErr := saveState(m.cfg.SubscriptionFile(), stateCopy); saveErr != nil {
		m.log.Warn("состояние подписки не сохранено", "err", saveErr)
	}
}

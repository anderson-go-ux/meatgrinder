package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"log"
	"math"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
)

// Constants
const (
	FieldWidth                 = 800
	FieldHeight                = 600
	TickRate                   = 30 // Times per second the server processes updates
	UpdateRate                 = 10 // Times per second the client renders the screen, can be different from tick rate
	PlayerRadius               = 20
	DamageRadius               = 50
	PlayerAttackSpeed          = 1.0
	DamageResistanceMultiplier = 2.0 // Урон уменьшается в 2 раза для устойчивых персонажей
	EventPlayerJoined          = "player_joined"
	EventPlayerLeft            = "player_left"
	EventPlayerDamage          = "player_damage"
	EventPlayerDeath           = "player_death"
	EventPlayerRespawn         = "player_respawn"
	EventPlayerAttack          = "player_attack"
	EventSplashDamage          = "splash_damage"
	MaxBots                    = 5   // Максимальное количество ботов
	BotUpdateRate              = 2.0 // Частота обновления направления ботов (раз в секунду)
	AttackRangeWarrior         = 50  // Радиус атаки для воина
	AttackRangeMage            = 200 // Радиус атаки для мага
	MaxDamageDistance          = 50  // Расстояние максимального урона
	MinDamageMultiplier        = 0.2 // Минимальный множитель урона (20% на максимальной дистанции)
)

// Types of characters
const (
	WarriorClass = iota
	MageClass
	TotalClasses
)

var ClassNames = map[int]string{
	WarriorClass: "Warrior",
	MageClass:    "Mage",
}

var ClassColors = map[int]color.RGBA{
	WarriorClass: {255, 0, 0, 255}, // Red
	MageClass:    {0, 0, 255, 255}, // Blue
}

// Types of damage
const (
	PhysicalDamage = iota
	MagicalDamage
)

// LogEntry struct
type LogEntry struct {
	Timestamp time.Time              `json:"timestamp"`
	EventType string                 `json:"event"`
	Data      map[string]interface{} `json:"data"`
}

// Game state structures
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type PlayerState struct {
	ID              int       `json:"id"`
	Class           int       `json:"class"`
	Position        Point     `json:"position"`
	Health          float64   `json:"health"`
	Target          int       `json:"target"`
	LastAttackTime  time.Time `json:"last_attack_time"`
	MovingDirection Point     `json:"moving_direction"`
}

type WorldState struct {
	Players map[int]*PlayerState `json:"players"`
}

// Player actions
type PlayerAction struct {
	ActionType   string `json:"action_type"`   // "move", "attack"
	Target       Point  `json:"target"`        // only for move
	AttackTarget int    `json:"attack_target"` // only for attack
	Direction    Point  `json:"direction"`     // only for move
}

// Network messages
type NetworkMessage struct {
	MessageType string      `json:"message_type"` // "state", "action"
	Data        interface{} `json:"data"`
}

// Game state
type Game struct {
	mu             sync.Mutex
	worldState     WorldState
	logEntries     []LogEntry
	serverMode     bool
	serverConn     net.Conn
	clientConn     net.Conn
	nextPlayerID   int
	lastUpdateTime time.Time
	inputAction    chan PlayerAction
	playerID       int

	// UI state
	playerPositions   map[int]Point
	playerConnections map[int]net.Conn
	bots              map[int]*Bot // ID игрока -> бот
}

var ClassStats = map[int]struct {
	MoveSpeed    float64
	AttackSpeed  float64
	AttackDamage float64
}{
	WarriorClass: {
		MoveSpeed:    100,
		AttackSpeed:  1.0,
		AttackDamage: 15.0,
	},
	MageClass: {
		MoveSpeed:    80,
		AttackSpeed:  0.8,
		AttackDamage: 20.0,
	},
}

// Добавим структуру для ботов
type Bot struct {
	LastDirectionChange time.Time
}

func NewGame(serverMode bool) *Game {
	rand.Seed(time.Now().UnixNano())
	g := &Game{
		worldState: WorldState{
			Players: make(map[int]*PlayerState),
		},
		logEntries:        make([]LogEntry, 0),
		serverMode:        serverMode,
		nextPlayerID:      1,
		lastUpdateTime:    time.Now(),
		inputAction:       make(chan PlayerAction, 10),
		playerPositions:   make(map[int]Point),
		playerConnections: make(map[int]net.Conn),
		bots:              make(map[int]*Bot),
	}

	if serverMode {
		g.playerID = 0
		go g.spawnBots()
	} else {
		g.playerID = -1
	}

	return g
}

// Добавим функцию для создания ботов
func (g *Game) spawnBots() {
	time.Sleep(2 * time.Second) // Ждем немного для подключения реальных игроков

	g.mu.Lock()
	defer g.mu.Unlock()

	// Проверяем текущее количество ботов
	currentBots := len(g.bots)
	if currentBots >= MaxBots {
		return
	}

	// Создаем только недостающее количество ботов
	for i := 0; i < MaxBots-currentBots; i++ {
		botID := g.nextPlayerID
		g.nextPlayerID++

		// Случайный класс и позиция
		playerClass := rand.Intn(TotalClasses)
		pos := Point{X: rand.Float64() * FieldWidth, Y: rand.Float64() * FieldHeight}

		g.worldState.Players[botID] = &PlayerState{
			ID:              botID,
			Class:           playerClass,
			Position:        pos,
			Health:          100,
			Target:          0,
			LastAttackTime:  time.Now(),
			MovingDirection: Point{X: 0, Y: 0},
		}
		g.playerPositions[botID] = pos
		g.bots[botID] = &Bot{
			LastDirectionChange: time.Now(),
		}
	}
}

// --- Server Logic ---
func (g *Game) StartServer() {
	ln, err := net.Listen("tcp", ":8080")
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	log.Println("Server listening on :8080")

	go g.serverTick()

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println("Error accepting connection:", err)
			continue
		}
		log.Println("Accepted new client")
		go g.handleClient(conn)
	}
}

func (g *Game) handleClient(conn net.Conn) {
	defer func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		for playerID, playerConn := range g.playerConnections {
			if playerConn == conn {
				conn.Close()
				delete(g.playerConnections, playerID)
				break
			}
		}

	}()

	playerID := g.addPlayer()
	g.mu.Lock()
	g.playerConnections[playerID] = conn
	g.mu.Unlock()

	g.sendInitialState(conn, playerID)

	decoder := json.NewDecoder(conn)
	for {
		var msg NetworkMessage
		err := decoder.Decode(&msg)
		if err != nil {
			log.Printf("Error decoding message: %v", err)
			g.removePlayer(playerID)
			return
		}

		if msg.MessageType == "action" {
			var action PlayerAction
			data, ok := msg.Data.(map[string]interface{})
			if !ok {
				log.Println("Error invalid message data:", data)
				continue
			}

			action.ActionType = data["action_type"].(string)

			if action.ActionType == "move" {
				if target, ok := data["target"].(map[string]interface{}); ok {
					action.Target.X = target["x"].(float64)
					action.Target.Y = target["y"].(float64)
				}
				if dir, ok := data["direction"].(map[string]interface{}); ok {
					action.Direction.X = dir["x"].(float64)
					action.Direction.Y = dir["y"].(float64)
				}
				g.mu.Lock()
				if player, ok := g.worldState.Players[playerID]; ok {
					player.MovingDirection = action.Direction
					g.playerPositions[playerID] = player.Position
				}
				g.mu.Unlock()
				select {
				case g.inputAction <- action:
				default:
					// Если канал полон, пропускаем
				}
			} else if action.ActionType == "attack" {
				if attackTarget, ok := data["attack_target"].(float64); ok {
					action.AttackTarget = int(attackTarget)
				}
			}
			g.mu.Lock()
			if player, ok := g.worldState.Players[playerID]; ok {
				player.Target = action.AttackTarget
			}
			g.mu.Unlock()
		}
	}
}

func (g *Game) addPlayer() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	playerID := g.nextPlayerID
	g.nextPlayerID++

	// Random class
	playerClass := rand.Intn(TotalClasses)

	// Random position
	pos := Point{X: rand.Float64() * FieldWidth, Y: rand.Float64() * FieldHeight}

	g.worldState.Players[playerID] = &PlayerState{
		ID:              playerID,
		Class:           playerClass,
		Position:        pos,
		Health:          100,
		Target:          0, // No target by default
		LastAttackTime:  time.Now(),
		MovingDirection: Point{X: 0, Y: 0},
	}
	g.playerPositions[playerID] = pos

	logEntry := LogEntry{
		Timestamp: time.Now(),
		EventType: "player_joined",
		Data: map[string]interface{}{
			"player_id": playerID,
			"class":     ClassNames[playerClass],
			"position":  pos,
		},
	}
	g.logEntries = append(g.logEntries, logEntry)
	log.Printf("Player %d joined, class: %v, position: %v\n", playerID, ClassNames[playerClass], pos)
	return playerID
}

func (g *Game) removePlayer(playerID int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.worldState.Players[playerID]; ok {
		logEntry := LogEntry{
			Timestamp: time.Now(),
			EventType: "player_left",
			Data: map[string]interface{}{
				"player_id": playerID,
			},
		}
		g.logEntries = append(g.logEntries, logEntry)
		delete(g.worldState.Players, playerID)
		delete(g.playerPositions, playerID)
		delete(g.playerConnections, playerID)
		log.Printf("Player %d disconnected\n", playerID)
	}
}

func (g *Game) serverTick() {
	ticker := time.NewTicker(time.Second / TickRate)
	defer ticker.Stop()
	for range ticker.C {
		g.updateGameState()
		g.broadcastState()
	}
}

func (g *Game) updateGameState() {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	deltaTime := now.Sub(g.lastUpdateTime).Seconds()
	g.lastUpdateTime = now

	// Обновляем поведение ботов
	for id, bot := range g.bots {
		if player, ok := g.worldState.Players[id]; ok {
			// Меняем направление движения бота каждые BotUpdateRate секунд
			if now.Sub(bot.LastDirectionChange).Seconds() >= 1.0/BotUpdateRate {
				// Случайное направление
				angle := rand.Float64() * 2 * math.Pi
				player.MovingDirection = Point{
					X: math.Cos(angle),
					Y: math.Sin(angle),
				}
				bot.LastDirectionChange = now

				// Находим ближайшую цель
				var closestDist float64 = math.MaxFloat64
				var closestID int
				for targetID, target := range g.worldState.Players {
					if targetID == id {
						continue
					}
					dist := math.Sqrt(math.Pow(player.Position.X-target.Position.X, 2) +
						math.Pow(player.Position.Y-target.Position.Y, 2))
					if dist < closestDist {
						closestDist = dist
						closestID = targetID
					}
				}
				if closestID != 0 {
					player.Target = closestID
				}
			}
		}
	}

	for id, player := range g.worldState.Players {
		// Movement
		if player.MovingDirection.X != 0 || player.MovingDirection.Y != 0 {
			speed := ClassStats[player.Class].MoveSpeed
			player.Position.X += player.MovingDirection.X * speed * deltaTime
			player.Position.Y += player.MovingDirection.Y * speed * deltaTime

			// Clamp to field
			player.Position.X = math.Max(0, math.Min(player.Position.X, FieldWidth))
			player.Position.Y = math.Max(0, math.Min(player.Position.Y, FieldHeight))

			// Обновляем позицию в playerPositions
			g.playerPositions[id] = player.Position
		}

		// Attack
		if player.Target != 0 {
			targetPlayer, ok := g.worldState.Players[player.Target]
			if !ok {
				continue // Target is invalid
			}

			if now.Sub(player.LastAttackTime).Seconds() >= 1.0/PlayerAttackSpeed {
				g.performAttack(player, targetPlayer, now)
				player.LastAttackTime = now
			}
		}
	}

	// Respawn dead players
	for id, player := range g.worldState.Players {
		if player.Health <= 0 {
			log.Printf("Player %d died.\n", id)

			logEntry := LogEntry{
				Timestamp: time.Now(),
				EventType: "player_died",
				Data: map[string]interface{}{
					"player_id": id,
				},
			}
			g.logEntries = append(g.logEntries, logEntry)

			// Respawn
			player.Health = 100
			player.Position.X = rand.Float64() * FieldWidth
			player.Position.Y = rand.Float64() * FieldHeight

			logEntry = LogEntry{
				Timestamp: time.Now(),
				EventType: "player_respawned",
				Data: map[string]interface{}{
					"player_id": id,
					"position":  player.Position,
				},
			}
			g.logEntries = append(g.logEntries, logEntry)

			log.Printf("Player %d respawned at %v\n", id, player.Position)
		}
	}
}

func (g *Game) performAttack(attacker *PlayerState, target *PlayerState, now time.Time) {
	// Базовый урон из характеристик класса
	baseDamage := ClassStats[attacker.Class].AttackDamage
	damageType := PhysicalDamage
	if attacker.Class == MageClass {
		damageType = MagicalDamage
	}

	// Расчет расстояния до цели
	dist := math.Sqrt(math.Pow(attacker.Position.X-target.Position.X, 2) +
		math.Pow(attacker.Position.Y-target.Position.Y, 2))

	// Расчет множителя урона в зависимости от расстояния
	distanceMultiplier := 1.0
	if dist > MaxDamageDistance {
		// Линейное уменьшение урона с расстоянием
		distanceMultiplier = math.Max(MinDamageMultiplier,
			1.0-((dist-MaxDamageDistance)/MaxDamageDistance)*(1.0-MinDamageMultiplier))
	}

	// Расчет сопротивления урону
	resistanceMultiplier := 1.0
	if (target.Class == WarriorClass && damageType == PhysicalDamage) ||
		(target.Class == MageClass && damageType == MagicalDamage) {
		resistanceMultiplier = 1.0 / DamageResistanceMultiplier
	}

	// Применяем все множители к базовому урону
	finalDamage := baseDamage * distanceMultiplier * resistanceMultiplier
	target.Health -= finalDamage
	if target.Health < 0 {
		target.Health = 0
	}

	logEntry := LogEntry{
		Timestamp: now,
		EventType: "player_attack",
		Data: map[string]interface{}{
			"attacker_id": attacker.ID,
			"target_id":   target.ID,
			"damage":      finalDamage,
			"damage_type": damageType,
		},
	}
	g.logEntries = append(g.logEntries, logEntry)
	log.Printf("Player %d attacked Player %d for %.2f damage\n", attacker.ID, target.ID, finalDamage)

	// Apply splash damage
	for _, other := range g.worldState.Players {
		if other.ID == target.ID {
			continue
		}

		dist := math.Sqrt(math.Pow(target.Position.X-other.Position.X, 2) + math.Pow(target.Position.Y-other.Position.Y, 2))
		if dist < DamageRadius {

			otherReduction := 1.0
			if (other.Class == WarriorClass && damageType == PhysicalDamage) || (other.Class == MageClass && damageType == MagicalDamage) {
				otherReduction = 0.5 // Resist
			}
			splashDamage := finalDamage * otherReduction
			other.Health -= splashDamage
			if other.Health < 0 {
				other.Health = 0
			}

			logEntry = LogEntry{
				Timestamp: now,
				EventType: "splash_damage",
				Data: map[string]interface{}{
					"attacker_id": attacker.ID,
					"target_id":   other.ID,
					"damage":      splashDamage,
					"damage_type": damageType,
				},
			}
			g.logEntries = append(g.logEntries, logEntry)
			log.Printf("Player %d received %.2f splash damage from Player %d\n", other.ID, splashDamage, attacker.ID)
		}
	}
}

func (g *Game) broadcastState() {
	g.mu.Lock()
	defer g.mu.Unlock()

	state := NetworkMessage{
		MessageType: "state",
		Data:        g.worldState,
	}

	for _, player := range g.worldState.Players {
		if g.serverMode {
			if conn, ok := g.playerConnections[player.ID]; ok {
				g.mu.Unlock()
				if err := json.NewEncoder(conn).Encode(state); err != nil {
					log.Printf("Error encoding state for player %d: %v\n", player.ID, err)
				}
				g.mu.Lock()
			}
		} else if player.ID == g.playerID {
			if g.clientConn == nil {
				continue
			}
			if err := json.NewEncoder(g.clientConn).Encode(state); err != nil {
				log.Printf("Error encoding state for client: %v\n", err)
			}
		}
	}
}

func (g *Game) getPlayerConnection(playerID int) (net.Conn, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	conn, ok := g.playerConnections[playerID]
	return conn, ok
}

func (g *Game) sendInitialState(conn net.Conn, playerID int) {
	initialState := NetworkMessage{
		MessageType: "init",
		Data: map[string]interface{}{
			"player_id":   playerID,
			"server_mode": g.serverMode,
		},
	}
	if err := json.NewEncoder(conn).Encode(initialState); err != nil {
		log.Println("Error sending initial state:", err)
	}

	state := NetworkMessage{
		MessageType: "state",
		Data:        g.worldState,
	}

	if err := json.NewEncoder(conn).Encode(state); err != nil {
		log.Println("Error sending state:", err)
	}

	log.Printf("Sent initial state to player %d\n", playerID)

}

// --- Client Logic ---

func (g *Game) StartClient() {
	ebiten.SetWindowSize(FieldWidth, FieldHeight)
	ebiten.SetWindowTitle("Meat Grinder")

	conn, err := net.Dial("tcp", "localhost:8080")
	if err != nil {
		log.Fatal("Failed to connect to server:", err)
	}
	g.clientConn = conn
	log.Println("Connected to server")

	go g.clientReceive()

	if err := ebiten.RunGame(g); err != nil {
		log.Fatal(err)
	}
}

func (g *Game) clientReceive() {
	decoder := json.NewDecoder(g.clientConn)

	var initMsg NetworkMessage
	if err := decoder.Decode(&initMsg); err != nil {
		log.Println("Error decoding init message:", err)
		return
	}

	if initMsg.MessageType != "init" {
		log.Println("Expected 'init' message, but got:", initMsg.MessageType)
		return
	}

	data, ok := initMsg.Data.(map[string]interface{})
	if !ok {
		log.Println("Error invalid message data in init message:", initMsg.Data)
		return
	}

	if id, ok := data["player_id"].(float64); ok {
		g.playerID = int(id)
		log.Println("Assigned player ID:", g.playerID)
	}

	var stateMsg NetworkMessage
	if err := decoder.Decode(&stateMsg); err != nil {
		log.Println("Error decoding state message:", err)
		return
	}

	if stateMsg.MessageType != "state" {
		log.Println("Expected 'state' message, but got:", stateMsg.MessageType)
		return
	}

	stateData, ok := stateMsg.Data.(map[string]interface{})
	if !ok {
		log.Println("Error invalid state data:", stateMsg.Data)
		return
	}

	stateJSON, err := json.Marshal(stateData)
	if err != nil {
		log.Println("Error marshaling state data to json:", err)
		return
	}

	g.mu.Lock()
	err = json.Unmarshal(stateJSON, &g.worldState)
	if err != nil {
		log.Println("Error unmarshaling world state:", err)
	}
	// Обновляем позиции после получения нового состояния
	for id, player := range g.worldState.Players {
		g.playerPositions[id] = player.Position
	}
	g.mu.Unlock()

	for {
		var msg NetworkMessage
		err := decoder.Decode(&msg)
		if err != nil {
			log.Println("Error decoding message:", err)
			return
		}

		if msg.MessageType == "state" {
			stateData, ok := msg.Data.(map[string]interface{})
			if !ok {
				log.Println("Error invalid state data:", msg.Data)
				continue
			}

			stateJSON, err := json.Marshal(stateData)
			if err != nil {
				log.Println("Error marshaling state data to json:", err)
				continue
			}

			g.mu.Lock()
			err = json.Unmarshal(stateJSON, &g.worldState)
			if err != nil {
				log.Println("Error unmarshaling world state:", err)
			}
			// Обновляем позиции после получения нового состояния
			for id, player := range g.worldState.Players {
				g.playerPositions[id] = player.Position
			}
			g.mu.Unlock()
		}
	}
}

// Update implements ebiten.Game interface
func (g *Game) Update() error {
	g.handleInput()
	return nil
}

func (g *Game) handleInput() {
	if g.serverMode {
		return
	}

	g.mu.Lock()
	// Проверяем только существование игрока, переменная не нужна
	if _, ok := g.worldState.Players[g.playerID]; !ok {
		g.mu.Unlock()
		return // Player hasn't joined yet
	}
	g.mu.Unlock()

	var direction Point

	// Movement Input
	if ebiten.IsKeyPressed(ebiten.KeyW) {
		direction.Y -= 1
	}
	if ebiten.IsKeyPressed(ebiten.KeyS) {
		direction.Y += 1
	}
	if ebiten.IsKeyPressed(ebiten.KeyA) {
		direction.X -= 1
	}
	if ebiten.IsKeyPressed(ebiten.KeyD) {
		direction.X += 1
	}

	// Normalize
	magnitude := math.Sqrt(direction.X*direction.X + direction.Y*direction.Y)
	if magnitude > 0 {
		direction.X /= magnitude
		direction.Y /= magnitude
	}

	g.mu.Lock()
	if player, ok := g.worldState.Players[g.playerID]; ok {
		if direction.X != player.MovingDirection.X || direction.Y != player.MovingDirection.Y {
			// Обновляем локальное направление
			player.MovingDirection = direction
			// Отправляем на сервер
			g.sendActionToServer(PlayerAction{
				ActionType: "move",
				Direction:  direction,
			})
		}
	}
	g.mu.Unlock()

	// Attack Input
	if inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonLeft) {
		x, y := ebiten.CursorPosition()
		closestPlayer := g.findClosestPlayer(Point{X: float64(x), Y: float64(y)})

		if closestPlayer != 0 {
			g.mu.Lock()
			if p, ok := g.worldState.Players[g.playerID]; ok {
				p.Target = closestPlayer
			}
			g.mu.Unlock()

			g.sendActionToServer(PlayerAction{
				ActionType:   "attack",
				AttackTarget: closestPlayer,
			})
		}
	}
}

func (g *Game) findClosestPlayer(mousePos Point) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	var closestPlayer int
	minDistance := math.MaxFloat64

	if len(g.worldState.Players) <= 1 {
		return 0
	}

	// Определяем радиус атаки текущего игрока
	currentPlayer := g.worldState.Players[g.playerID]
	if currentPlayer == nil {
		return 0
	}

	attackRange := AttackRangeWarrior
	if currentPlayer.Class == MageClass {
		attackRange = AttackRangeMage
	}

	for _, player := range g.worldState.Players {
		if player.ID == g.playerID {
			continue
		}

		dist := math.Sqrt(math.Pow(mousePos.X-player.Position.X, 2) + math.Pow(mousePos.Y-player.Position.Y, 2))
		// Проверяем, находится ли цель в радиусе атаки
		if dist <= float64(attackRange) && dist < minDistance {
			minDistance = dist
			closestPlayer = player.ID
		}
	}

	return closestPlayer
}

func (g *Game) sendActionToServer(action PlayerAction) {
	msg := NetworkMessage{
		MessageType: "action",
		Data:        action,
	}
	if g.clientConn == nil {
		return
	}
	err := json.NewEncoder(g.clientConn).Encode(msg)
	if err != nil {
		log.Println("Error sending action:", err)
	}
}

// Draw implements ebiten.Game interface
func (g *Game) Draw(screen *ebiten.Image) {
	g.mu.Lock()
	defer g.mu.Unlock()
	screen.Fill(hexToRGBA(0x2b2b2b))

	// Отрисовка игроков
	for _, player := range g.worldState.Players {
		playerColor := ClassColors[player.Class]
		playerPos := g.playerPositions[player.ID]

		// Рисуем игрока
		ebitenutil.DrawCircle(screen, playerPos.X, playerPos.Y, PlayerRadius, playerColor)

		// Рисуем имя, класс и здоровье
		text := fmt.Sprintf("%s %d/%d", ClassNames[player.Class], int(player.Health), 100)
		ebitenutil.DebugPrintAt(screen, text, int(playerPos.X)-20, int(playerPos.Y)-30)

		if g.playerID == player.ID && !g.serverMode {
			ebitenutil.DebugPrintAt(screen, "You", int(playerPos.X)-10, int(playerPos.Y)+30)
		}

		// Рисуем линию к цели и подсветку цели
		if player.Target != 0 {
			if target, ok := g.worldState.Players[player.Target]; ok {
				targetPos := g.playerPositions[target.ID]
				ebitenutil.DrawLine(screen, playerPos.X, playerPos.Y, targetPos.X, targetPos.Y, color.RGBA{255, 255, 255, 128})
				ebitenutil.DrawCircle(screen, targetPos.X, targetPos.Y, PlayerRadius+5, color.RGBA{255, 0, 0, 64})
			}
		}

		// Для ботов рисуем метку
		if _, isBot := g.bots[player.ID]; isBot {
			ebitenutil.DebugPrintAt(screen, "[BOT]", int(playerPos.X)-15, int(playerPos.Y)-45)
		}
	}
}

func (g *Game) Layout(outsideWidth, outsideHeight int) (int, int) {
	return FieldWidth, FieldHeight
}

func hexToRGBA(hex int) color.RGBA {
	r := uint8((hex >> 16) & 0xFF)
	g := uint8((hex >> 8) & 0xFF)
	b := uint8(hex & 0xFF)
	return color.RGBA{r, g, b, 0xff}
}

func main() {
	serverMode := os.Getenv("SERVER") == "1"
	game := NewGame(serverMode)

	if serverMode {
		game.StartServer()
	} else {
		game.StartClient()
	}
}

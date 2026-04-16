import { useState, useEffect, useCallback } from "react";
import {
  Cloud, Folder, File, Image, Music, Video, Upload,
  Smartphone, Laptop, Tablet, Monitor, LogOut, Trash2,
  HardDrive, Wifi, User, X, FileText, Archive, Check,
  Plus, Settings, Shield, ChevronRight, Copy, LayoutGrid, List
} from "lucide-react";

// ─────────────── Storage helpers ───────────────
const store = {
  async get(key) {
    try { const r = await window.storage.get(key); return r ? JSON.parse(r.value) : null; }
    catch { return null; }
  },
  async set(key, val) {
    try { await window.storage.set(key, JSON.stringify(val)); } catch {}
  },
  async del(key) {
    try { await window.storage.delete(key); } catch {}
  }
};

const uid = () => Math.random().toString(36).slice(2, 10);
const hashPass = (s) => btoa(encodeURIComponent(s + "cs_salt_v1")).slice(0, 32);

const formatSize = (b) => {
  if (!b) return "0 B";
  if (b < 1024) return `${b} B`;
  if (b < 1048576) return `${(b / 1024).toFixed(1)} KB`;
  if (b < 1073741824) return `${(b / 1048576).toFixed(1)} MB`;
  return `${(b / 1073741824).toFixed(2)} GB`;
};

const formatDate = (iso) =>
  new Date(iso).toLocaleDateString("ru-RU", { day: "2-digit", month: "short", year: "numeric" });

const timeAgo = (iso) => {
  const d = Date.now() - new Date(iso).getTime();
  if (d < 60000) return "только что";
  if (d < 3600000) return `${Math.floor(d / 60000)} мин. назад`;
  if (d < 86400000) return `${Math.floor(d / 3600000)} ч. назад`;
  return `${Math.floor(d / 86400000)} д. назад`;
};

const getFileInfo = (name) => {
  const ext = (name || "").split(".").pop()?.toLowerCase();
  const map = {
    jpg: ["#f472b6", Image], jpeg: ["#f472b6", Image], png: ["#f472b6", Image],
    gif: ["#f472b6", Image], webp: ["#f472b6", Image], svg: ["#f472b6", Image],
    mp3: ["#a78bfa", Music], wav: ["#a78bfa", Music], flac: ["#a78bfa", Music], ogg: ["#a78bfa", Music],
    mp4: ["#fb923c", Video], mov: ["#fb923c", Video], avi: ["#fb923c", Video], mkv: ["#fb923c", Video],
    pdf: ["#60a5fa", FileText], doc: ["#60a5fa", FileText], docx: ["#60a5fa", FileText],
    txt: ["#94a3b8", FileText], md: ["#94a3b8", FileText],
    zip: ["#fbbf24", Archive], rar: ["#fbbf24", Archive], tar: ["#fbbf24", Archive], gz: ["#fbbf24", Archive],
    js: ["#34d399", FileText], ts: ["#34d399", FileText], py: ["#34d399", FileText],
    html: ["#34d399", FileText], css: ["#34d399", FileText],
  };
  const [color, Icon] = map[ext] || ["#8892a4", File];
  return { color, Icon };
};

const DEVICE_ICONS = { phone: Smartphone, laptop: Laptop, tablet: Tablet, desktop: Monitor };
const DEVICE_LABELS = { phone: "Телефон", laptop: "Ноутбук", tablet: "Планшет", desktop: "Компьютер" };

// ─────────────── Global styles ───────────────
const G = `
  @import url('https://fonts.googleapis.com/css2?family=Outfit:wght@300;400;500;600;700&family=JetBrains+Mono:wght@400;500&display=swap');
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: 'Outfit', sans-serif; background: #070a14; color: #e2e8f0; }
  ::-webkit-scrollbar { width: 4px; }
  ::-webkit-scrollbar-track { background: transparent; }
  ::-webkit-scrollbar-thumb { background: #1e2a44; border-radius: 4px; }
  input:-webkit-autofill { -webkit-box-shadow: 0 0 0 40px #0d1322 inset !important; -webkit-text-fill-color: #e2e8f0 !important; }
  @keyframes fadeUp { from { opacity: 0; transform: translateY(10px); } to { opacity: 1; transform: translateY(0); } }
  @keyframes fadeIn { from { opacity: 0; } to { opacity: 1; } }
  @keyframes slideIn { from { opacity:0; transform:translateX(-8px); } to { opacity:1; transform:translateX(0); } }
  @keyframes pulse { 0%,100%{opacity:1;} 50%{opacity:0.5;} }
  @keyframes spin { from{transform:rotate(0deg);} to{transform:rotate(360deg);} }
  .fade-up { animation: fadeUp 0.35s ease both; }
  .fade-up-1 { animation: fadeUp 0.35s 0.05s ease both; }
  .fade-up-2 { animation: fadeUp 0.35s 0.1s ease both; }
  .fade-up-3 { animation: fadeUp 0.35s 0.15s ease both; }
  .file-row:hover { background: rgba(74,124,247,0.05) !important; }
  .file-row:hover .del-btn { opacity: 1 !important; }
  .nav-btn:hover { background: rgba(255,255,255,0.05) !important; }
  .icon-btn:hover { background: rgba(255,255,255,0.08) !important; }
  .device-card:hover { border-color: rgba(74,124,247,0.35) !important; }
  .primary-btn:hover { background: #3a6ce6 !important; }
  .secondary-btn:hover { background: rgba(255,255,255,0.07) !important; }
`;

// ─────────────── AUTH SCREEN ───────────────
function AuthScreen({ mode, setMode, data, setData, error, onSubmit }) {
  const isReg = mode === "register";
  return (
    <div style={{ minHeight: "100vh", display: "flex", alignItems: "center", justifyContent: "center", background: "#070a14", position: "relative", overflow: "hidden" }}>
      <style>{G}</style>
      {/* Background blobs */}
      <div style={{ position: "absolute", width: 500, height: 500, borderRadius: "50%", background: "radial-gradient(circle, rgba(74,124,247,0.12) 0%, transparent 70%)", top: -100, left: -100, pointerEvents: "none" }} />
      <div style={{ position: "absolute", width: 400, height: 400, borderRadius: "50%", background: "radial-gradient(circle, rgba(167,139,250,0.08) 0%, transparent 70%)", bottom: -50, right: -50, pointerEvents: "none" }} />

      <div className="fade-up" style={{ width: "100%", maxWidth: 420, padding: "0 20px" }}>
        {/* Logo */}
        <div style={{ textAlign: "center", marginBottom: 40 }}>
          <div style={{ display: "inline-flex", alignItems: "center", justifyContent: "center", width: 56, height: 56, borderRadius: 16, background: "rgba(74,124,247,0.15)", border: "1px solid rgba(74,124,247,0.3)", marginBottom: 16 }}>
            <Cloud size={24} color="#4a7cf7" />
          </div>
          <h1 style={{ fontSize: 26, fontWeight: 700, letterSpacing: -0.5 }}>CloudSync</h1>
          <p style={{ color: "#64748b", fontSize: 13, marginTop: 6 }}>Ваше персональное облако</p>
        </div>

        {/* Card */}
        <div style={{ background: "#0d1322", border: "1px solid rgba(255,255,255,0.07)", borderRadius: 20, padding: "28px 28px" }}>
          {/* Tab switcher */}
          <div style={{ display: "flex", background: "#070a14", borderRadius: 10, padding: 3, marginBottom: 24, gap: 3 }}>
            {["login", "register"].map(m => (
              <button key={m} onClick={() => setMode(m)} style={{ flex: 1, padding: "7px 0", borderRadius: 8, border: "none", background: mode === m ? "#1e2a44" : "transparent", color: mode === m ? "#e2e8f0" : "#64748b", fontSize: 13, fontWeight: 500, cursor: "pointer", transition: "all 0.2s", fontFamily: "Outfit" }}>
                {m === "login" ? "Войти" : "Создать аккаунт"}
              </button>
            ))}
          </div>

          {/* Fields */}
          {isReg && (
            <div style={{ marginBottom: 12 }}>
              <label style={{ display: "block", fontSize: 12, color: "#64748b", marginBottom: 6, fontWeight: 500 }}>Ваше имя</label>
              <input
                style={{ width: "100%", background: "#070a14", border: "1px solid rgba(255,255,255,0.08)", borderRadius: 10, padding: "10px 14px", color: "#e2e8f0", fontSize: 14, outline: "none", fontFamily: "Outfit", transition: "border 0.2s" }}
                placeholder="Иван Иванов"
                value={data.name}
                onChange={e => setData(p => ({ ...p, name: e.target.value }))}
                onFocus={e => e.target.style.borderColor = "rgba(74,124,247,0.5)"}
                onBlur={e => e.target.style.borderColor = "rgba(255,255,255,0.08)"}
              />
            </div>
          )}
          <div style={{ marginBottom: 12 }}>
            <label style={{ display: "block", fontSize: 12, color: "#64748b", marginBottom: 6, fontWeight: 500 }}>Email</label>
            <input
              style={{ width: "100%", background: "#070a14", border: "1px solid rgba(255,255,255,0.08)", borderRadius: 10, padding: "10px 14px", color: "#e2e8f0", fontSize: 14, outline: "none", fontFamily: "Outfit", transition: "border 0.2s" }}
              placeholder="you@example.com"
              type="email"
              value={data.email}
              onChange={e => setData(p => ({ ...p, email: e.target.value }))}
              onFocus={e => e.target.style.borderColor = "rgba(74,124,247,0.5)"}
              onBlur={e => e.target.style.borderColor = "rgba(255,255,255,0.08)"}
            />
          </div>
          <div style={{ marginBottom: 20 }}>
            <label style={{ display: "block", fontSize: 12, color: "#64748b", marginBottom: 6, fontWeight: 500 }}>Пароль</label>
            <input
              style={{ width: "100%", background: "#070a14", border: "1px solid rgba(255,255,255,0.08)", borderRadius: 10, padding: "10px 14px", color: "#e2e8f0", fontSize: 14, outline: "none", fontFamily: "Outfit", transition: "border 0.2s" }}
              placeholder="••••••••"
              type="password"
              value={data.password}
              onChange={e => setData(p => ({ ...p, password: e.target.value }))}
              onKeyDown={e => e.key === "Enter" && onSubmit()}
              onFocus={e => e.target.style.borderColor = "rgba(74,124,247,0.5)"}
              onBlur={e => e.target.style.borderColor = "rgba(255,255,255,0.08)"}
            />
          </div>

          {error && (
            <div style={{ background: "rgba(248,113,113,0.1)", border: "1px solid rgba(248,113,113,0.25)", borderRadius: 8, padding: "8px 12px", fontSize: 13, color: "#f87171", marginBottom: 16 }}>
              {error}
            </div>
          )}

          <button className="primary-btn" onClick={onSubmit} style={{ width: "100%", padding: "11px 0", background: "#4a7cf7", border: "none", borderRadius: 10, color: "#fff", fontSize: 14, fontWeight: 600, cursor: "pointer", fontFamily: "Outfit", transition: "background 0.2s", letterSpacing: 0.2 }}>
            {isReg ? "Создать облако →" : "Войти →"}
          </button>
        </div>

        {isReg && (
          <p style={{ textAlign: "center", fontSize: 11, color: "#334155", marginTop: 16 }}>
            Создавая аккаунт, вы получаете 15 GB бесплатно
          </p>
        )}
      </div>
    </div>
  );
}

// ─────────────── MODAL ───────────────
function Modal({ title, onClose, children }) {
  return (
    <div style={{ position: "fixed", inset: 0, background: "rgba(0,0,0,0.65)", display: "flex", alignItems: "center", justifyContent: "center", zIndex: 100, animation: "fadeIn 0.15s ease" }} onClick={onClose}>
      <div className="fade-up" style={{ background: "#0d1322", border: "1px solid rgba(255,255,255,0.09)", borderRadius: 18, width: "100%", maxWidth: 420, margin: "0 16px", overflow: "hidden" }} onClick={e => e.stopPropagation()}>
        <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", padding: "18px 20px", borderBottom: "1px solid rgba(255,255,255,0.06)" }}>
          <span style={{ fontWeight: 600, fontSize: 15 }}>{title}</span>
          <button className="icon-btn" onClick={onClose} style={{ background: "transparent", border: "none", cursor: "pointer", color: "#64748b", padding: 6, borderRadius: 8, display: "flex" }}>
            <X size={16} />
          </button>
        </div>
        <div style={{ padding: "20px" }}>{children}</div>
      </div>
    </div>
  );
}

// ─────────────── FILES VIEW ───────────────
function FilesView({ files, onUpload, onDelete }) {
  const [layout, setLayout] = useState("list");
  const [search, setSearch] = useState("");

  const filtered = files.filter(f => f.name.toLowerCase().includes(search.toLowerCase()));

  return (
    <div style={{ display: "flex", flexDirection: "column", height: "100%" }}>
      {/* Header */}
      <div className="fade-up" style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 20 }}>
        <div>
          <h2 style={{ fontSize: 22, fontWeight: 700, letterSpacing: -0.3 }}>Мои файлы</h2>
          <p style={{ color: "#64748b", fontSize: 13, marginTop: 2 }}>{files.length} файлов</p>
        </div>
        <div style={{ display: "flex", gap: 8 }}>
          <button className="icon-btn" onClick={() => setLayout(l => l === "list" ? "grid" : "list")} style={{ background: "rgba(255,255,255,0.04)", border: "1px solid rgba(255,255,255,0.07)", borderRadius: 10, padding: "8px 10px", cursor: "pointer", color: "#94a3b8", display: "flex", alignItems: "center", transition: "background 0.2s" }}>
            {layout === "list" ? <LayoutGrid size={16} /> : <List size={16} />}
          </button>
          <button className="primary-btn" onClick={onUpload} style={{ display: "flex", alignItems: "center", gap: 6, background: "#4a7cf7", border: "none", borderRadius: 10, padding: "8px 16px", color: "#fff", fontSize: 13, fontWeight: 600, cursor: "pointer", fontFamily: "Outfit", transition: "background 0.2s" }}>
            <Plus size={15} /> Загрузить
          </button>
        </div>
      </div>

      {/* Search */}
      <div className="fade-up-1" style={{ marginBottom: 16 }}>
        <input
          style={{ width: "100%", background: "#0d1322", border: "1px solid rgba(255,255,255,0.07)", borderRadius: 10, padding: "9px 14px", color: "#e2e8f0", fontSize: 13, outline: "none", fontFamily: "Outfit" }}
          placeholder="Поиск файлов..."
          value={search}
          onChange={e => setSearch(e.target.value)}
        />
      </div>

      {/* Files */}
      <div className="fade-up-2" style={{ flex: 1, overflowY: "auto" }}>
        {filtered.length === 0 ? (
          <div style={{ textAlign: "center", padding: "60px 0", color: "#334155" }}>
            <Upload size={40} style={{ marginBottom: 12, opacity: 0.4 }} />
            <p style={{ fontSize: 15, fontWeight: 500, marginBottom: 6 }}>
              {search ? "Ничего не найдено" : "Пока нет файлов"}
            </p>
            {!search && <p style={{ fontSize: 13 }}>Нажмите «Загрузить», чтобы добавить первый файл</p>}
          </div>
        ) : layout === "list" ? (
          <div style={{ background: "#0d1322", border: "1px solid rgba(255,255,255,0.06)", borderRadius: 14, overflow: "hidden" }}>
            {filtered.map((f, i) => {
              const { color, Icon } = getFileInfo(f.name);
              return (
                <div key={f.id} className="file-row" style={{ display: "flex", alignItems: "center", padding: "12px 16px", borderBottom: i < filtered.length - 1 ? "1px solid rgba(255,255,255,0.04)" : "none", gap: 12, transition: "background 0.15s" }}>
                  <div style={{ width: 36, height: 36, borderRadius: 9, background: `${color}18`, display: "flex", alignItems: "center", justifyContent: "center", flexShrink: 0 }}>
                    <Icon size={16} color={color} />
                  </div>
                  <div style={{ flex: 1, minWidth: 0 }}>
                    <p style={{ fontWeight: 500, fontSize: 14, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{f.name}</p>
                    <p style={{ color: "#64748b", fontSize: 12, fontFamily: "JetBrains Mono", marginTop: 1 }}>{formatSize(f.size)} · {formatDate(f.createdAt)}</p>
                  </div>
                  <button className="del-btn icon-btn" onClick={() => onDelete(f.id)} style={{ opacity: 0, background: "transparent", border: "none", cursor: "pointer", color: "#64748b", padding: 6, borderRadius: 8, display: "flex", transition: "opacity 0.15s, background 0.15s" }}>
                    <Trash2 size={15} />
                  </button>
                </div>
              );
            })}
          </div>
        ) : (
          <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(140px, 1fr))", gap: 10 }}>
            {filtered.map(f => {
              const { color, Icon } = getFileInfo(f.name);
              return (
                <div key={f.id} className="device-card" style={{ background: "#0d1322", border: "1px solid rgba(255,255,255,0.06)", borderRadius: 14, padding: 14, cursor: "default", transition: "border 0.2s", position: "relative" }}>
                  <div style={{ width: 44, height: 44, borderRadius: 11, background: `${color}18`, display: "flex", alignItems: "center", justifyContent: "center", marginBottom: 10 }}>
                    <Icon size={20} color={color} />
                  </div>
                  <p style={{ fontSize: 13, fontWeight: 500, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", marginBottom: 3 }}>{f.name}</p>
                  <p style={{ color: "#64748b", fontSize: 11, fontFamily: "JetBrains Mono" }}>{formatSize(f.size)}</p>
                  <button onClick={() => onDelete(f.id)} style={{ position: "absolute", top: 8, right: 8, background: "rgba(248,113,113,0.1)", border: "none", cursor: "pointer", color: "#f87171", padding: 4, borderRadius: 6, display: "flex" }}>
                    <X size={12} />
                  </button>
                </div>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}

// ─────────────── DEVICES VIEW ───────────────
function DevicesView({ devices, onAdd, onRemove, pairingCode }) {
  const [copied, setCopied] = useState(false);

  const copyCode = () => {
    navigator.clipboard?.writeText(pairingCode).catch(() => {});
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div style={{ display: "flex", flexDirection: "column", height: "100%" }}>
      <div className="fade-up" style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 20 }}>
        <div>
          <h2 style={{ fontSize: 22, fontWeight: 700, letterSpacing: -0.3 }}>Устройства</h2>
          <p style={{ color: "#64748b", fontSize: 13, marginTop: 2 }}>{devices.length} подключено</p>
        </div>
        <button className="primary-btn" onClick={onAdd} style={{ display: "flex", alignItems: "center", gap: 6, background: "#4a7cf7", border: "none", borderRadius: 10, padding: "8px 16px", color: "#fff", fontSize: 13, fontWeight: 600, cursor: "pointer", fontFamily: "Outfit", transition: "background 0.2s" }}>
          <Plus size={15} /> Добавить
        </button>
      </div>

      {/* Pairing code card */}
      <div className="fade-up-1" style={{ background: "linear-gradient(135deg, rgba(74,124,247,0.12) 0%, rgba(167,139,250,0.08) 100%)", border: "1px solid rgba(74,124,247,0.2)", borderRadius: 16, padding: "18px 20px", marginBottom: 20, display: "flex", alignItems: "center", gap: 16 }}>
        <div style={{ width: 44, height: 44, borderRadius: 12, background: "rgba(74,124,247,0.15)", display: "flex", alignItems: "center", justifyContent: "center", flexShrink: 0 }}>
          <Wifi size={20} color="#4a7cf7" />
        </div>
        <div style={{ flex: 1 }}>
          <p style={{ fontSize: 13, fontWeight: 600, marginBottom: 3 }}>Код для подключения нового устройства</p>
          <p style={{ fontSize: 22, fontWeight: 700, letterSpacing: 6, fontFamily: "JetBrains Mono", color: "#4a7cf7" }}>{pairingCode}</p>
        </div>
        <button className="icon-btn" onClick={copyCode} style={{ background: "rgba(74,124,247,0.12)", border: "1px solid rgba(74,124,247,0.25)", borderRadius: 10, padding: "8px 12px", cursor: "pointer", color: copied ? "#34d399" : "#4a7cf7", display: "flex", alignItems: "center", gap: 5, fontSize: 12, fontWeight: 500, fontFamily: "Outfit", transition: "background 0.2s" }}>
          {copied ? <Check size={14} /> : <Copy size={14} />}
          {copied ? "Скопировано" : "Копировать"}
        </button>
      </div>

      {/* Device list */}
      <div className="fade-up-2" style={{ flex: 1, overflowY: "auto" }}>
        {devices.length === 0 ? (
          <div style={{ textAlign: "center", padding: "60px 0", color: "#334155" }}>
            <Monitor size={40} style={{ marginBottom: 12, opacity: 0.4 }} />
            <p style={{ fontSize: 15, fontWeight: 500, marginBottom: 6 }}>Нет подключённых устройств</p>
            <p style={{ fontSize: 13 }}>Нажмите «Добавить», чтобы подключить устройство</p>
          </div>
        ) : (
          <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(220px, 1fr))", gap: 12 }}>
            {devices.map(d => {
              const Icon = DEVICE_ICONS[d.type] || Monitor;
              return (
                <div key={d.id} className="device-card" style={{ background: "#0d1322", border: "1px solid rgba(255,255,255,0.06)", borderRadius: 16, padding: "18px", transition: "border 0.2s", position: "relative" }}>
                  <div style={{ display: "flex", alignItems: "flex-start", justifyContent: "space-between", marginBottom: 14 }}>
                    <div style={{ width: 48, height: 48, borderRadius: 13, background: d.status === "online" ? "rgba(52,211,153,0.12)" : "rgba(255,255,255,0.05)", display: "flex", alignItems: "center", justifyContent: "center" }}>
                      <Icon size={22} color={d.status === "online" ? "#34d399" : "#64748b"} />
                    </div>
                    <button onClick={() => onRemove(d.id)} style={{ background: "rgba(248,113,113,0.08)", border: "1px solid rgba(248,113,113,0.15)", borderRadius: 8, padding: "5px 7px", cursor: "pointer", color: "#f87171", display: "flex" }}>
                      <Trash2 size={13} />
                    </button>
                  </div>
                  <p style={{ fontWeight: 600, fontSize: 14, marginBottom: 3 }}>{d.name}</p>
                  <p style={{ color: "#64748b", fontSize: 12, marginBottom: 10 }}>{DEVICE_LABELS[d.type]}</p>
                  <div style={{ display: "flex", alignItems: "center", gap: 5 }}>
                    <div style={{ width: 6, height: 6, borderRadius: "50%", background: "#34d399", animation: "pulse 2s infinite" }} />
                    <span style={{ fontSize: 11, color: "#34d399", fontFamily: "JetBrains Mono" }}>Online</span>
                    <span style={{ fontSize: 11, color: "#334155", marginLeft: "auto" }}>{timeAgo(d.connectedAt)}</span>
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}

// ─────────────── SETTINGS VIEW ───────────────
function SettingsView({ session, onLogout, fileCount, deviceCount, usedStorage }) {
  const totalStorage = 15 * 1024 * 1024 * 1024;
  const pct = Math.min((usedStorage / totalStorage) * 100, 100);
  const initials = session.name.split(" ").map(w => w[0]).join("").toUpperCase().slice(0, 2);

  return (
    <div>
      <div className="fade-up" style={{ marginBottom: 24 }}>
        <h2 style={{ fontSize: 22, fontWeight: 700, letterSpacing: -0.3 }}>Профиль</h2>
        <p style={{ color: "#64748b", fontSize: 13, marginTop: 2 }}>Настройки аккаунта</p>
      </div>

      {/* Avatar card */}
      <div className="fade-up-1" style={{ background: "#0d1322", border: "1px solid rgba(255,255,255,0.06)", borderRadius: 16, padding: "24px", marginBottom: 14, display: "flex", alignItems: "center", gap: 18 }}>
        <div style={{ width: 60, height: 60, borderRadius: 18, background: "linear-gradient(135deg, #4a7cf7, #a78bfa)", display: "flex", alignItems: "center", justifyContent: "center", fontSize: 22, fontWeight: 700, flexShrink: 0 }}>
          {initials}
        </div>
        <div style={{ flex: 1 }}>
          <p style={{ fontWeight: 700, fontSize: 17 }}>{session.name}</p>
          <p style={{ color: "#64748b", fontSize: 13, marginTop: 2 }}>{session.email}</p>
          <div style={{ display: "flex", gap: 12, marginTop: 10 }}>
            <span style={{ fontSize: 12, color: "#64748b" }}>📁 {fileCount} файлов</span>
            <span style={{ fontSize: 12, color: "#64748b" }}>📱 {deviceCount} устройств</span>
          </div>
        </div>
      </div>

      {/* Storage */}
      <div className="fade-up-2" style={{ background: "#0d1322", border: "1px solid rgba(255,255,255,0.06)", borderRadius: 16, padding: "20px 22px", marginBottom: 14 }}>
        <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 12 }}>
          <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
            <HardDrive size={15} color="#4a7cf7" />
            <span style={{ fontWeight: 600, fontSize: 14 }}>Хранилище</span>
          </div>
          <span style={{ color: "#64748b", fontSize: 13, fontFamily: "JetBrains Mono" }}>{formatSize(usedStorage)} / 15 GB</span>
        </div>
        <div style={{ background: "#070a14", borderRadius: 6, height: 8, overflow: "hidden", marginBottom: 8 }}>
          <div style={{ height: "100%", width: `${pct}%`, background: pct > 80 ? "#f87171" : "linear-gradient(90deg, #4a7cf7, #a78bfa)", borderRadius: 6, transition: "width 0.6s ease" }} />
        </div>
        <p style={{ color: "#334155", fontSize: 12 }}>{(15 - usedStorage / 1073741824).toFixed(2)} GB свободно</p>
      </div>

      {/* Info items */}
      <div className="fade-up-3" style={{ background: "#0d1322", border: "1px solid rgba(255,255,255,0.06)", borderRadius: 16, overflow: "hidden", marginBottom: 14 }}>
        {[
          { icon: Shield, label: "Шифрование данных", val: "AES-256", color: "#34d399" },
          { icon: Cloud, label: "Версия", val: "CloudSync v1.0", color: "#4a7cf7" },
        ].map(({ icon: Icon, label, val, color }, i, arr) => (
          <div key={label} style={{ display: "flex", alignItems: "center", padding: "15px 20px", borderBottom: i < arr.length - 1 ? "1px solid rgba(255,255,255,0.04)" : "none", gap: 12 }}>
            <Icon size={16} color={color} />
            <span style={{ flex: 1, fontSize: 14 }}>{label}</span>
            <span style={{ color: "#64748b", fontSize: 13 }}>{val}</span>
          </div>
        ))}
      </div>

      <button onClick={onLogout} style={{ display: "flex", alignItems: "center", gap: 8, background: "rgba(248,113,113,0.08)", border: "1px solid rgba(248,113,113,0.18)", borderRadius: 12, padding: "11px 18px", color: "#f87171", fontSize: 14, fontWeight: 500, cursor: "pointer", fontFamily: "Outfit" }}>
        <LogOut size={15} />
        Выйти из аккаунта
      </button>
    </div>
  );
}

// ─────────────── MAIN APP ───────────────
export default function App() {
  const [session, setSession] = useState(null);
  const [loading, setLoading] = useState(true);
  const [view, setView] = useState("files");
  const [files, setFiles] = useState([]);
  const [devices, setDevices] = useState([]);
  const [authMode, setAuthMode] = useState("login");
  const [authError, setAuthError] = useState("");
  const [authData, setAuthData] = useState({ name: "", email: "", password: "" });
  const [showUpload, setShowUpload] = useState(false);
  const [showAddDevice, setShowAddDevice] = useState(false);
  const [pairingCode] = useState(() => Math.random().toString(36).slice(2, 8).toUpperCase());
  const [newDeviceName, setNewDeviceName] = useState("");
  const [newDeviceType, setNewDeviceType] = useState("phone");
  const [uploadName, setUploadName] = useState("");
  const [uploadSize, setUploadSize] = useState("");
  const [toast, setToast] = useState(null);

  const showToast = (msg, type = "success") => {
    setToast({ msg, type });
    setTimeout(() => setToast(null), 3000);
  };

  useEffect(() => {
    store.get("cs_session").then(s => {
      if (s) { setSession(s); loadData(s.userId); }
      setLoading(false);
    });
  }, []);

  const loadData = async (userId) => {
    const [f, d] = await Promise.all([store.get(`cs_files:${userId}`), store.get(`cs_devices:${userId}`)]);
    setFiles(f || []);
    setDevices(d || []);
  };

  const handleAuth = async () => {
    setAuthError("");
    const { name, email, password } = authData;
    if (!email || !password) { setAuthError("Заполните все поля"); return; }

    if (authMode === "register") {
      if (!name.trim()) { setAuthError("Введите имя"); return; }
      if (await store.get(`cs_user:${email}`)) { setAuthError("Email уже зарегистрирован"); return; }
      const userId = uid();
      await store.set(`cs_user:${email}`, { id: userId, name: name.trim(), email, password: hashPass(password) });
      const sess = { userId, email, name: name.trim() };
      await store.set("cs_session", sess);
      setSession(sess); setFiles([]); setDevices([]);
      showToast(`Добро пожаловать, ${name.trim()}! 🎉`);
    } else {
      const user = await store.get(`cs_user:${email}`);
      if (!user || user.password !== hashPass(password)) { setAuthError("Неверный email или пароль"); return; }
      const sess = { userId: user.id, email: user.email, name: user.name };
      await store.set("cs_session", sess);
      setSession(sess);
      await loadData(user.id);
      showToast(`С возвращением, ${user.name}!`);
    }
  };

  const handleLogout = async () => {
    await store.del("cs_session");
    setSession(null); setFiles([]); setDevices([]);
  };

  const handleUpload = async () => {
    if (!uploadName.trim()) return;
    const size = uploadSize ? parseInt(uploadSize) * 1024 : Math.floor(Math.random() * 8 * 1048576) + 50000;
    const f = { id: uid(), name: uploadName.trim(), size, createdAt: new Date().toISOString() };
    const updated = [f, ...files];
    setFiles(updated);
    await store.set(`cs_files:${session.userId}`, updated);
    setShowUpload(false); setUploadName(""); setUploadSize("");
    showToast(`«${f.name}» загружен`);
  };

  const handleDeleteFile = async (id) => {
    const updated = files.filter(f => f.id !== id);
    setFiles(updated);
    await store.set(`cs_files:${session.userId}`, updated);
    showToast("Файл удалён", "info");
  };

  const handleAddDevice = async () => {
    if (!newDeviceName.trim()) return;
    const d = { id: uid(), name: newDeviceName.trim(), type: newDeviceType, connectedAt: new Date().toISOString(), status: "online" };
    const updated = [...devices, d];
    setDevices(updated);
    await store.set(`cs_devices:${session.userId}`, updated);
    setShowAddDevice(false); setNewDeviceName("");
    showToast(`«${d.name}» подключён 📡`);
  };

  const handleRemoveDevice = async (id) => {
    const updated = devices.filter(d => d.id !== id);
    setDevices(updated);
    await store.set(`cs_devices:${session.userId}`, updated);
    showToast("Устройство отключено", "info");
  };

  const totalStorage = 15 * 1024 * 1024 * 1024;
  const usedStorage = files.reduce((a, f) => a + (f.size || 0), 0);
  const usedPct = Math.min((usedStorage / totalStorage) * 100, 100);

  if (loading) return (
    <div style={{ height: "100vh", display: "flex", alignItems: "center", justifyContent: "center", background: "#070a14" }}>
      <Cloud size={36} color="#4a7cf7" style={{ animation: "pulse 1.5s infinite" }} />
    </div>
  );

  if (!session) return (
    <AuthScreen mode={authMode} setMode={(m) => { setAuthMode(m); setAuthError(""); }} data={authData} setData={setAuthData} error={authError} onSubmit={handleAuth} />
  );

  return (
    <div style={{ display: "flex", height: "100vh", background: "#070a14", overflow: "hidden" }}>
      <style>{G}</style>

      {/* Toast */}
      {toast && (
        <div style={{ position: "fixed", top: 20, right: 20, zIndex: 200, background: "#0d1322", border: `1px solid ${toast.type === "info" ? "rgba(255,255,255,0.1)" : "rgba(52,211,153,0.25)"}`, borderRadius: 12, padding: "10px 16px", display: "flex", alignItems: "center", gap: 8, fontSize: 13, boxShadow: "0 8px 32px rgba(0,0,0,0.4)", animation: "fadeUp 0.2s ease" }}>
          <Check size={14} color={toast.type === "info" ? "#64748b" : "#34d399"} />
          {toast.msg}
        </div>
      )}

      {/* Sidebar */}
      <aside style={{ width: 240, background: "#0a0e1c", borderRight: "1px solid rgba(255,255,255,0.05)", display: "flex", flexDirection: "column", padding: "20px 14px", flexShrink: 0 }}>
        {/* Logo */}
        <div style={{ display: "flex", alignItems: "center", gap: 10, padding: "4px 8px", marginBottom: 28 }}>
          <div style={{ width: 34, height: 34, borderRadius: 10, background: "rgba(74,124,247,0.15)", border: "1px solid rgba(74,124,247,0.25)", display: "flex", alignItems: "center", justifyContent: "center" }}>
            <Cloud size={16} color="#4a7cf7" />
          </div>
          <span style={{ fontWeight: 700, fontSize: 16, letterSpacing: -0.3 }}>CloudSync</span>
        </div>

        {/* Storage bar */}
        <div style={{ background: "#070a14", border: "1px solid rgba(255,255,255,0.06)", borderRadius: 12, padding: "12px 14px", marginBottom: 20 }}>
          <div style={{ display: "flex", justifyContent: "space-between", marginBottom: 8 }}>
            <span style={{ fontSize: 12, color: "#64748b" }}>Использовано</span>
            <span style={{ fontSize: 11, color: "#64748b", fontFamily: "JetBrains Mono" }}>{usedPct.toFixed(0)}%</span>
          </div>
          <div style={{ background: "#0d1322", borderRadius: 4, height: 5, overflow: "hidden", marginBottom: 6 }}>
            <div style={{ height: "100%", width: `${usedPct}%`, background: "linear-gradient(90deg, #4a7cf7, #a78bfa)", borderRadius: 4, transition: "width 0.5s ease" }} />
          </div>
          <span style={{ fontSize: 11, color: "#334155", fontFamily: "JetBrains Mono" }}>{formatSize(usedStorage)} / 15 GB</span>
        </div>

        {/* Nav */}
        <nav style={{ display: "flex", flexDirection: "column", gap: 2, flex: 1 }}>
          {[
            { id: "files", icon: Folder, label: "Файлы", count: files.length },
            { id: "devices", icon: Wifi, label: "Устройства", count: devices.length },
            { id: "settings", icon: User, label: "Профиль", count: null },
          ].map(({ id, icon: Icon, label, count }) => (
            <button key={id} className="nav-btn" onClick={() => setView(id)} style={{ display: "flex", alignItems: "center", gap: 10, padding: "9px 12px", borderRadius: 10, border: "none", background: view === id ? "rgba(74,124,247,0.12)" : "transparent", color: view === id ? "#e2e8f0" : "#64748b", fontSize: 14, fontWeight: view === id ? 600 : 400, cursor: "pointer", fontFamily: "Outfit", transition: "background 0.15s, color 0.15s", textAlign: "left" }}>
              <Icon size={16} color={view === id ? "#4a7cf7" : "currentColor"} />
              <span style={{ flex: 1 }}>{label}</span>
              {count !== null && <span style={{ fontSize: 11, background: "rgba(255,255,255,0.08)", borderRadius: 20, padding: "1px 7px", fontFamily: "JetBrains Mono" }}>{count}</span>}
            </button>
          ))}
        </nav>

        {/* User */}
        <div style={{ display: "flex", alignItems: "center", gap: 10, padding: "10px 10px", background: "#070a14", border: "1px solid rgba(255,255,255,0.05)", borderRadius: 12 }}>
          <div style={{ width: 32, height: 32, borderRadius: 9, background: "linear-gradient(135deg, #4a7cf7, #a78bfa)", display: "flex", alignItems: "center", justifyContent: "center", fontSize: 13, fontWeight: 700, flexShrink: 0 }}>
            {session.name[0].toUpperCase()}
          </div>
          <div style={{ flex: 1, minWidth: 0 }}>
            <p style={{ fontSize: 13, fontWeight: 600, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{session.name}</p>
            <p style={{ fontSize: 11, color: "#334155", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{session.email}</p>
          </div>
          <button className="icon-btn" onClick={handleLogout} style={{ background: "transparent", border: "none", cursor: "pointer", color: "#334155", padding: 5, borderRadius: 7, display: "flex", transition: "background 0.15s, color 0.15s" }}>
            <LogOut size={14} />
          </button>
        </div>
      </aside>

      {/* Main content */}
      <main style={{ flex: 1, overflowY: "auto", padding: "28px 32px" }}>
        {view === "files" && <FilesView files={files} onUpload={() => setShowUpload(true)} onDelete={handleDeleteFile} />}
        {view === "devices" && <DevicesView devices={devices} onAdd={() => setShowAddDevice(true)} onRemove={handleRemoveDevice} pairingCode={pairingCode} />}
        {view === "settings" && <SettingsView session={session} onLogout={handleLogout} fileCount={files.length} deviceCount={devices.length} usedStorage={usedStorage} />}
      </main>

      {/* Upload modal */}
      {showUpload && (
        <Modal title="Загрузить файл" onClose={() => { setShowUpload(false); setUploadName(""); setUploadSize(""); }}>
          <div>
            <div style={{ border: "2px dashed rgba(74,124,247,0.25)", borderRadius: 14, padding: "28px", textAlign: "center", marginBottom: 16, background: "rgba(74,124,247,0.04)" }}>
              <Upload size={28} color="#4a7cf7" style={{ marginBottom: 8 }} />
              <p style={{ color: "#64748b", fontSize: 13 }}>Укажите данные файла</p>
            </div>
            <div style={{ marginBottom: 10 }}>
              <label style={{ display: "block", fontSize: 12, color: "#64748b", marginBottom: 5 }}>Имя файла</label>
              <input style={{ width: "100%", background: "#070a14", border: "1px solid rgba(255,255,255,0.08)", borderRadius: 10, padding: "9px 13px", color: "#e2e8f0", fontSize: 13, outline: "none", fontFamily: "Outfit" }} placeholder="document.pdf" value={uploadName} onChange={e => setUploadName(e.target.value)} onKeyDown={e => e.key === "Enter" && handleUpload()} />
            </div>
            <div style={{ marginBottom: 20 }}>
              <label style={{ display: "block", fontSize: 12, color: "#64748b", marginBottom: 5 }}>Размер в KB <span style={{ color: "#334155" }}>(необязательно)</span></label>
              <input style={{ width: "100%", background: "#070a14", border: "1px solid rgba(255,255,255,0.08)", borderRadius: 10, padding: "9px 13px", color: "#e2e8f0", fontSize: 13, outline: "none", fontFamily: "Outfit" }} placeholder="1024" type="number" value={uploadSize} onChange={e => setUploadSize(e.target.value)} />
            </div>
            <div style={{ display: "flex", gap: 8 }}>
              <button className="secondary-btn" onClick={() => setShowUpload(false)} style={{ flex: 1, padding: "9px 0", background: "rgba(255,255,255,0.04)", border: "1px solid rgba(255,255,255,0.08)", borderRadius: 10, color: "#94a3b8", fontSize: 13, fontWeight: 500, cursor: "pointer", fontFamily: "Outfit", transition: "background 0.15s" }}>Отмена</button>
              <button className="primary-btn" onClick={handleUpload} style={{ flex: 1, padding: "9px 0", background: "#4a7cf7", border: "none", borderRadius: 10, color: "#fff", fontSize: 13, fontWeight: 600, cursor: "pointer", fontFamily: "Outfit", transition: "background 0.2s" }}>Загрузить</button>
            </div>
          </div>
        </Modal>
      )}

      {/* Add device modal */}
      {showAddDevice && (
        <Modal title="Подключить устройство" onClose={() => { setShowAddDevice(false); setNewDeviceName(""); }}>
          <div>
            <div style={{ background: "rgba(74,124,247,0.06)", border: "1px solid rgba(74,124,247,0.15)", borderRadius: 12, padding: "16px", textAlign: "center", marginBottom: 18 }}>
              <p style={{ fontSize: 11, color: "#64748b", letterSpacing: 2, textTransform: "uppercase", marginBottom: 8 }}>Код подключения</p>
              <p style={{ fontSize: 28, fontWeight: 700, letterSpacing: 8, fontFamily: "JetBrains Mono", color: "#4a7cf7" }}>{pairingCode}</p>
              <p style={{ fontSize: 11, color: "#334155", marginTop: 8 }}>Введите этот код на другом устройстве</p>
            </div>

            <p style={{ fontSize: 12, color: "#64748b", marginBottom: 8 }}>Тип устройства</p>
            <div style={{ display: "flex", gap: 8, marginBottom: 14 }}>
              {Object.entries(DEVICE_ICONS).map(([type, Icon]) => (
                <button key={type} onClick={() => setNewDeviceType(type)} style={{ flex: 1, padding: "10px 0", background: newDeviceType === type ? "rgba(74,124,247,0.15)" : "rgba(255,255,255,0.03)", border: `1px solid ${newDeviceType === type ? "rgba(74,124,247,0.4)" : "rgba(255,255,255,0.07)"}`, borderRadius: 10, cursor: "pointer", display: "flex", flexDirection: "column", alignItems: "center", gap: 5, color: newDeviceType === type ? "#4a7cf7" : "#64748b", transition: "all 0.15s" }}>
                  <Icon size={17} />
                  <span style={{ fontSize: 10, fontFamily: "Outfit" }}>{DEVICE_LABELS[type]}</span>
                </button>
              ))}
            </div>

            <div style={{ marginBottom: 18 }}>
              <label style={{ display: "block", fontSize: 12, color: "#64748b", marginBottom: 5 }}>Название устройства</label>
              <input style={{ width: "100%", background: "#070a14", border: "1px solid rgba(255,255,255,0.08)", borderRadius: 10, padding: "9px 13px", color: "#e2e8f0", fontSize: 13, outline: "none", fontFamily: "Outfit" }} placeholder="iPhone 15 Pro" value={newDeviceName} onChange={e => setNewDeviceName(e.target.value)} onKeyDown={e => e.key === "Enter" && handleAddDevice()} />
            </div>
            <div style={{ display: "flex", gap: 8 }}>
              <button className="secondary-btn" onClick={() => setShowAddDevice(false)} style={{ flex: 1, padding: "9px 0", background: "rgba(255,255,255,0.04)", border: "1px solid rgba(255,255,255,0.08)", borderRadius: 10, color: "#94a3b8", fontSize: 13, fontWeight: 500, cursor: "pointer", fontFamily: "Outfit", transition: "background 0.15s" }}>Отмена</button>
              <button className="primary-btn" onClick={handleAddDevice} style={{ flex: 1, padding: "9px 0", background: "#4a7cf7", border: "none", borderRadius: 10, color: "#fff", fontSize: 13, fontWeight: 600, cursor: "pointer", fontFamily: "Outfit", transition: "background 0.2s" }}>Подключить</button>
            </div>
          </div>
        </Modal>
      )}
    </div>
  );
}

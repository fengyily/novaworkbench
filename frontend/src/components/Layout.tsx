import { useCallback, useEffect, useState } from 'react';
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom';
import { useAuth } from '../utils/auth';
import './Layout.css';

const navItems = [
  { to: '/', label: '📊 仪表盘', end: true, permission: 'menu.dashboard', shortLabel: '仪表盘', icon: '📊' },
  { to: '/projects', label: '📁 项目', end: false, permission: 'menu.projects', shortLabel: '项目', icon: '📁' },
  { to: '/requirements', label: '📋 需求', end: false, permission: 'menu.projects', shortLabel: '需求', icon: '📋' },
  { to: '/reports', label: '📝 周报', end: false, permission: 'menu.reports', shortLabel: '周报', icon: '📝' },
];

// NovaWorkbench mark — a deep-midnight rounded slab (the "workbench") with
// a bone-white "N" whose diagonal stroke is replaced by a three-dot
// constellation trailing up to a bright amber spark. The dark slab +
// warm-spark palette deliberately breaks from the indigo-gradient default
// that most dev-tool logos reach for, so the mark reads as its own object
// instead of dissolving into the indigo chrome (#4F46E5) used elsewhere.
function NovaLogo({ size = 28 }: { size?: number }) {
  return (
    <svg
      className="nw-mark"
      viewBox="0 0 28 28"
      width={size}
      height={size}
      role="img"
      aria-label="NovaWorkbench"
    >
      <defs>
        <linearGradient id="nw-slab" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0%" stopColor="#312E81" />
          <stop offset="55%" stopColor="#1E1B4B" />
          <stop offset="100%" stopColor="#0F172A" />
        </linearGradient>
        <linearGradient id="nw-spark" x1="0" y1="1" x2="1" y2="0">
          <stop offset="0%" stopColor="#FCD34D" />
          <stop offset="100%" stopColor="#F59E0B" />
        </linearGradient>
      </defs>
      {/* Slab — the workbench surface, with rounded corners and a faint
          inner stroke so it feels "set down" instead of "painted on". */}
      <rect x="1" y="1" width="26" height="26" rx="7" fill="url(#nw-slab)" />
      <rect
        x="1.5"
        y="1.5"
        width="25"
        height="25"
        rx="6.5"
        fill="none"
        stroke="#FBBF24"
        strokeOpacity="0.10"
      />
      {/* "N" — two pillars. The diagonal isn't drawn as a line; instead
          three stars fade in size from bottom-left to top-right, suggesting
          the trail of a nova's ignition. */}
      <path
        d="M 6 7 L 8.2 7 L 8.2 21 L 6 21 Z M 19.8 7 L 22 7 L 22 21 L 19.8 21 Z"
        fill="#F5F1E8"
      />
      {/* Constellation trail — small → medium → bright spark. Sizes and
          opacities step up so the eye reads motion from bottom-left to
          top-right, finishing on the brightest star. */}
      <circle cx="10.2" cy="17.8" r="1.1" fill="#FCD34D" fillOpacity="0.55" />
      <circle cx="13.5" cy="14.5" r="1.35" fill="#FCD34D" fillOpacity="0.85" />
      <circle cx="16.8" cy="11.2" r="1.7" fill="url(#nw-spark)" />
      {/* Spark highlight — a tiny inner dot to give the brightest star a
          "burning" quality instead of looking flat. */}
      <circle cx="16.8" cy="11.2" r="0.55" fill="#FFFBEB" />
    </svg>
  );
}

export default function Layout() {
  const { user, hasPermission, logout } = useAuth();
  const visibleItems = navItems.filter((item) => hasPermission(item.permission));
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const location = useLocation();
  const navigate = useNavigate();

  const closeSidebar = useCallback(() => setSidebarOpen(false), []);

  // ESC closes the mobile drawer (matches users' mental model from native
  // modals and saves them a tap on the small ✕ in the corner). The listener
  // is only attached while the drawer is open so we don't shadow other ESC
  // handlers (e.g. inline editing).
  useEffect(() => {
    if (!sidebarOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') closeSidebar();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [sidebarOpen, closeSidebar]);

  // Lock body scroll while the drawer overlay is up so the page underneath
  // doesn't scroll on touch. The single effect with cleanup handles both
  // unmount and toggle-to-closed — the else branch is redundant.
  useEffect(() => {
    if (!sidebarOpen) return;
    const body = document.body;
    // Only compensate on desktop where the scrollbar is visible.
    const sbw = window.innerWidth - document.documentElement.clientWidth;
    if (sbw > 0) body.style.setProperty('--scrollbar-comp', `${sbw}px`);
    body.classList.add('no-scroll');
    return () => {
      body.classList.remove('no-scroll');
      body.style.removeProperty('--scrollbar-comp');
    };
  }, [sidebarOpen]);

  // Edge-swipe gesture to open the drawer from the left edge. A real native
  // pattern on iOS Safari; we re-implement it lightly so the hamburger isn't
  // the only way to reach the full nav. Threshold: start within 24px of the
  // left edge, drag right by at least 50px, end in the leftmost 30% of the
  // screen. Listener is only attached on mobile widths.
  useEffect(() => {
    if (typeof window === 'undefined') return;
    const isMobile = () => window.matchMedia('(max-width: 768px)').matches;
    let startX = 0, startY = 0, tracking = false;
    const onStart = (e: TouchEvent) => {
      if (!isMobile() || sidebarOpen) return;
      const t = e.touches[0];
      if (t.clientX > 24) return;
      startX = t.clientX;
      startY = t.clientY;
      tracking = true;
    };
    const onMove = (e: TouchEvent) => {
      if (!tracking) return;
      // Cancel if the user is scrolling vertically instead of swiping.
      const t = e.touches[0];
      if (Math.abs(t.clientY - startY) > Math.abs(t.clientX - startX)) {
        tracking = false;
      }
    };
    const onEnd = (e: TouchEvent) => {
      if (!tracking) return;
      tracking = false;
      const t = e.changedTouches[0];
      const dx = t.clientX - startX;
      if (dx > 50 && t.clientX < window.innerWidth * 0.3) {
        setSidebarOpen(true);
      }
    };
    document.addEventListener('touchstart', onStart, { passive: true });
    document.addEventListener('touchmove', onMove, { passive: true });
    document.addEventListener('touchend', onEnd, { passive: true });
    return () => {
      document.removeEventListener('touchstart', onStart);
      document.removeEventListener('touchmove', onMove);
      document.removeEventListener('touchend', onEnd);
    };
  }, [sidebarOpen]);

  // Resolve the current section label for the mobile header title. The
  // longest match wins so "/projects/proj_xxx" resolves to "项目" not "/".
  // Detail pages show a back affordance instead of the section title.
  const sectionLabel = (() => {
    const path = location.pathname;
    if (path.startsWith('/requirements/')) return { label: '需求详情', back: '/requirements' };
    if (path.startsWith('/requirements')) return { label: '需求', back: null };
    if (path.startsWith('/projects/add')) return { label: '添加项目', back: '/projects' };
    if (path.startsWith('/projects/')) return { label: '项目详情', back: '/projects' };
    if (path.startsWith('/projects')) return { label: '项目', back: null };
    if (path.startsWith('/knowledge')) return { label: '知识库', back: null };
    if (path.startsWith('/chat')) return { label: '助手', back: null };
    if (path.startsWith('/reports')) return { label: '周报', back: null };
    if (path.startsWith('/settings')) return { label: '设置', back: null };
    if (path === '/') return { label: '仪表盘', back: null };
    return { label: '', back: null };
  })();

  const onMore = () => setSidebarOpen(true);

  return (
    <div className="app-layout">
      <header className="app-header">
        <div className="app-header-left">
          <button
            className="app-hamburger"
            aria-label="打开导航"
            onClick={() => setSidebarOpen((o) => !o)}
          >
            <span /><span /><span />
          </button>
          <span className="app-logo"><NovaLogo size={28} /><span className="app-logo-wordmark"><span className="app-logo-nova">Nova</span>Workbench</span></span>
          {/* Mobile-only: the section title lives inline next to the logo so
              the user always knows where they are without a separate title
              bar below the header. Hidden on desktop. */}
          {sectionLabel.label && (
            <span className="app-header-section">
              {sectionLabel.back && (
                <button
                  className="app-header-back"
                  aria-label="返回"
                  onClick={() => navigate(sectionLabel.back!)}
                >
                  <svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false">
                    <path
                      d="M15 5.5 L8.2 12 L15 18.5"
                      fill="none"
                      stroke="currentColor"
                      strokeWidth="2"
                      strokeLinecap="round"
                      strokeLinejoin="round"
                    />
                  </svg>
                </button>
              )}
              <span className="app-header-section-label">{sectionLabel.label}</span>
            </span>
          )}
        </div>
        <div className="app-header-right">
          {user && (
            <span className="app-header-user">
              {user.display_name || user.username}
              {user.is_admin && <span className="app-header-admin">管理员</span>}
            </span>
          )}
          {hasPermission('menu.settings') && (
            <a href="/settings" className="app-header-settings">⚙️ 设置</a>
          )}
          <button className="app-header-logout" onClick={() => logout()}>
            退出
          </button>
        </div>
      </header>
      <div className="app-body">
        {sidebarOpen && (
          <div className="sidebar-overlay" onClick={closeSidebar} aria-hidden="true" />
        )}
        <nav className={`app-sidebar${sidebarOpen ? ' sidebar-open' : ''}`}>
          <div className="sidebar-close-row">
            <span className="app-logo"><NovaLogo size={28} /><span className="app-logo-wordmark"><span className="app-logo-nova">Nova</span>Workbench</span></span>
            <button className="sidebar-close-btn" aria-label="关闭导航" onClick={closeSidebar}>✕</button>
          </div>
          {visibleItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) => `nav-item ${isActive ? 'active' : ''}`}
              onClick={closeSidebar}
            >
              {item.label}
            </NavLink>
          ))}
          {hasPermission('menu.chat') && (
            <NavLink to="/chat" className={({ isActive }) => `nav-item ${isActive ? 'active' : ''}`} onClick={closeSidebar}>
              <span className="nav-item-icon" aria-hidden="true">✨</span>
              <span>助手</span>
            </NavLink>
          )}
          {hasPermission('menu.settings') && (
            <NavLink to="/settings" className={({ isActive }) => `nav-item ${isActive ? 'active' : ''}`} onClick={closeSidebar}>
              ⚙️ 设置
            </NavLink>
          )}
        </nav>
        <main className="app-main">
          <Outlet />
        </main>
      </div>

      {/* Mobile-only bottom tab bar. The persistent thumb-zone navigation.
          Renders inside .app-layout so its position:fixed anchors to the
          viewport regardless of scroll. Hidden on desktop via CSS. */}
      <nav className="tab-bar" aria-label="主导航">
        {visibleItems.slice(0, 4).map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.end}
            className={({ isActive }) => `tab-bar-item ${isActive ? 'active' : ''}`}
          >
            <span className="tab-bar-icon">{item.icon}</span>
            <span className="tab-bar-label">{item.shortLabel}</span>
          </NavLink>
        ))}
        {/* The "更多" tab opens the sidebar drawer so the rest of the nav
            (AI 对话, 设置, etc.) is still one tap away. */}
        <button
          type="button"
          className="tab-bar-item more-tab"
          onClick={onMore}
          aria-label="更多"
        >
          <span className="tab-bar-icon">⋯</span>
          <span className="tab-bar-label">更多</span>
        </button>
      </nav>

      <footer className="app-footer">
        🟢 服务运行中 | localhost:9527 | v0.1.0
      </footer>
    </div>
  );
}

// Brand mark: two lanes crossing at different levels — the "立交" in the name.
export default function Logo({ size = 32 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 64 64" aria-hidden="true" className="logo">
      <defs>
        <linearGradient id="logo-bg" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="#1d2f6f" />
          <stop offset="1" stopColor="#165dff" />
        </linearGradient>
      </defs>
      <rect width="64" height="64" rx="15" fill="url(#logo-bg)" />
      <path d="M12 46 C 26 46, 38 18, 52 18" fill="none" stroke="#ffb65d" strokeWidth="7" strokeLinecap="round" />
      <path d="M12 18 C 26 18, 38 46, 52 46" fill="none" stroke="#1d3d9f" strokeWidth="11" strokeLinecap="round" />
      <path d="M12 18 C 26 18, 38 46, 52 46" fill="none" stroke="#e8f3ff" strokeWidth="6" strokeLinecap="round" />
    </svg>
  );
}

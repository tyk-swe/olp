const circle = (cx: number, cy: number, r: number) =>
  `M${cx - r} ${cy}a${r} ${r} 0 1 0 ${2 * r} 0a${r} ${r} 0 1 0 ${-2 * r} 0`;

const box = (x: number, y: number, w: number, h: number, r = 1.5) =>
  `M${x + r} ${y}h${w - 2 * r}a${r} ${r} 0 0 1 ${r} ${r}v${h - 2 * r}a${r} ${r} 0 0 1 ${-r} ${r}h${-(w - 2 * r)}a${r} ${r} 0 0 1 ${-r} ${-r}v${-(h - 2 * r)}a${r} ${r} 0 0 1 ${r} ${-r}z`;

export const ICONS = {
  overview: [
    box(3, 3, 7, 7),
    box(14, 3, 7, 7),
    box(3, 14, 7, 7),
    box(14, 14, 7, 7)
  ].join(''),
  provider: 'M7 7h10v10H7zM9 3v4m6-4v4M9 17v4m6-4v4M3 9h4m10 0h4M3 15h4m10 0h4',
  model: 'm12 3 8 4.5-8 4.5-8-4.5L12 3Zm-8 9 8 4.5 8-4.5M4 16.5l8 4.5 8-4.5',
  route: `${circle(5, 6, 2)}${circle(19, 6, 2)}${circle(19, 18, 2)}M7 6h3a4 4 0 0 1 4 4v4a4 4 0 0 0 3 3.9M14 10a4 4 0 0 1 3-3.9`,
  agent: `${box(5, 8, 14, 12, 2)}M12 4v4M9.5 13v2m5-2v2`,
  guardrail: 'M12 3 5 6v5c0 4.6 2.9 8.2 7 10 4.1-1.8 7-5.4 7-10V6l-7-3Z',
  key: `${circle(8, 15, 4)}m11 12 8-8m-3 3 2 2m-5 1 2 2`,
  access: `${circle(9, 8, 3)}M3.5 19a5.5 5.5 0 0 1 11 0M16 5.5a3 3 0 0 1 0 5.8M17 14a4.5 4.5 0 0 1 3.5 4.4`,
  request: 'M6 3h9l4 4v14H6zM14 3v5h5M9 13h7M9 17h5',
  usage: 'M4 20V10m5 10V4m6 16v-7m5 7V7',
  health: 'M3 12h4l2-6 4 12 2-6h6',
  audit: 'M7 3h10v4H7zM5 5H3v16h18V5h-2M8 12h8M8 16h6',
  playground:
    'm9 3-1 4-4 1 4 1 1 4 1-4 4-1-4-1-1-4ZM17 12l-.8 3.2L13 16l3.2.8L17 20l.8-3.2L21 16l-3.2-.8L17 12Z',
  settings: `${circle(12, 12, 3)}M19.4 15a1.7 1.7 0 0 0 .3 1.9l.1.1-2.8 2.8-.1-.1a1.7 1.7 0 0 0-1.9-.3 1.7 1.7 0 0 0-1 1.6v.2h-4V21a1.7 1.7 0 0 0-1-1.6 1.7 1.7 0 0 0-1.9.3l-.1.1L4.2 17l.1-.1a1.7 1.7 0 0 0 .3-1.9A1.7 1.7 0 0 0 3 14H2.8v-4H3a1.7 1.7 0 0 0 1.6-1 1.7 1.7 0 0 0-.3-1.9L4.2 7 7 4.2l.1.1a1.7 1.7 0 0 0 1.9.3A1.7 1.7 0 0 0 10 3V2.8h4V3a1.7 1.7 0 0 0 1 1.6 1.7 1.7 0 0 0 1.9-.3l.1-.1L19.8 7l-.1.1a1.7 1.7 0 0 0-.3 1.9 1.7 1.7 0 0 0 1.6 1h.2v4H21a1.7 1.7 0 0 0-1.6 1Z`,
  code: 'm8 7-5 5 5 5m8-10 5 5-5 5M14 4l-4 16',
  plugin: 'M9 3v4m6-4v4M6 7h12v4a6 6 0 0 1-12 0V7Zm6 10v4',
  server: `${box(4, 4, 16, 6)}${box(4, 14, 16, 6)}M8 7h.01M8 17h.01`,
  activity: `${circle(12, 12, 9)}M12 7v5l3 2`,
  search: `${circle(11, 11, 7)}m20 20-4-4`,
  link: 'M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1',
  folder:
    'M3 6a1 1 0 0 1 1-1h5l2 2h9a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V6Z',
  building:
    'M4 21V5l8-2v18M12 8h8v13M2 21h20M8 8v.01M8 12v.01M8 16v.01M16 12v.01M16 16v.01',
  id: `${box(3, 5, 18, 14, 2)}${circle(9, 11, 2)}M6 16a3 3 0 0 1 6 0M14 10h4m-4 4h3`,
  wallet: `${box(3, 6, 18, 14, 2)}M3 10h18M16 15h2M6 6l9-3 1 3`,
  tag: `M3 12V4a1 1 0 0 1 1-1h8l9 9-9 9-9-9Z${circle(7.5, 7.5, 1.5)}`,
  media: `${box(3, 5, 18, 14, 2)}m10 9 5 3-5 3v-6Z`,
  box: 'm12 3 8 4v10l-8 4-8-4V7l8-4Zm-8 4 8 4 8-4M12 11v10',
  bell: 'M6 16V11a6 6 0 0 1 12 0v5l2 2H4l2-2Zm4 4a2 2 0 0 0 4 0',
  export: 'M12 3v12m-5-5 5-5 5 5M4 17v3a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1v-3',
  plus: 'M12 5v14M5 12h14',
  more: `${circle(5, 12, 1)}${circle(12, 12, 1)}${circle(19, 12, 1)}`,
  menu: 'M4 7h16M4 12h16M4 17h16',
  close: 'M6 6l12 12M18 6 6 18',
  chevron: 'm8 10 4 4 4-4',
  arrow: 'M5 12h14m-5-5 5 5-5 5',
  sidebar: `${box(3, 4, 18, 16, 2)}M9 4v16`
} as const satisfies Record<string, string>;

export type IconName = keyof typeof ICONS;

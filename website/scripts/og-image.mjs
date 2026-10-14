// Renders public/og.png, the 1200x630 card that link previews (Open Graph,
// X/Twitter, Slack, LinkedIn) show for every page. Social sites do not render
// SVG, hence a PNG, committed so the site build needs no fonts. Run again
// after changing the text: node scripts/og-image.mjs (needs the Inter font).
import sharp from "sharp";
import { fileURLToPath } from "node:url";

const out = fileURLToPath(new URL("../public/og.png", import.meta.url));

const pills = ["Run", "Watch", "Score", "Reward"];
const pill = (label, i) => {
  const x = 96 + i * 168;
  return `<rect x="${x}" y="470" width="150" height="54" rx="27" fill="#ffffff" fill-opacity="0.08" stroke="#ffffff" stroke-opacity="0.25"/>
  <text x="${x + 75}" y="505" text-anchor="middle" font-size="24" font-weight="600" fill="#e5e7ff">${label}</text>`;
};

const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="630" viewBox="0 0 1200 630">
  <defs>
    <linearGradient id="bg" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="#0b0d1f"/><stop offset="1" stop-color="#11183a"/>
    </linearGradient>
    <linearGradient id="g" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="#6366f1"/><stop offset="1" stop-color="#14b8a6"/>
    </linearGradient>
  </defs>
  <rect width="1200" height="630" fill="url(#bg)"/>
  <circle cx="1060" cy="90" r="260" fill="url(#g)" opacity="0.18"/>
  <g font-family="Inter, sans-serif">
    <rect x="96" y="96" width="88" height="88" rx="24" fill="url(#g)"/>
    <path d="M118 141l18 18 29-38" fill="none" stroke="#fff" stroke-width="11" stroke-linecap="round" stroke-linejoin="round"/>
    <text x="212" y="166" font-size="72" font-weight="800" fill="#ffffff">Evals<tspan fill="#5eead4">.si</tspan></text>
    <text x="96" y="290" font-size="46" font-weight="700" fill="#ffffff">Evaluate AI before you ship,</text>
    <text x="96" y="350" font-size="46" font-weight="700" fill="#ffffff">while it runs, and as you train it</text>
    <text x="96" y="412" font-size="28" font-weight="500" fill="#a5b4fc">Agents · RAG · LLMs · RL training · pip install evalsi</text>
    ${pills.map(pill).join("\n    ")}
    <text x="1104" y="574" text-anchor="end" font-size="22" font-weight="500" fill="#94a3b8">github.com/Abhishek-Rnjn/Evals.si</text>
  </g>
</svg>`;

await sharp(Buffer.from(svg)).png({ compressionLevel: 9 }).toFile(out);
console.log(`wrote ${out}`);

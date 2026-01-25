FROM node:20-bookworm-slim

WORKDIR /app

COPY package*.json ./
RUN npm install --omit=dev

# ✅ playwright 브라우저 설치 (chromium만)
RUN npx playwright install --with-deps chromium

COPY . .

ENV PORT=3000
EXPOSE 3000

CMD ["node", "server.js"]

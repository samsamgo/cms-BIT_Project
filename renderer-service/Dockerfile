FROM mcr.microsoft.com/playwright:v1.48.0-jammy

WORKDIR /app

RUN apt-get update && apt-get install -y fonts-noto-cjk && rm -rf /var/lib/apt/lists/*

COPY package.json ./
RUN npm install --omit=dev

COPY server.js template.html ./

EXPOSE 3000
CMD ["npm", "start"]

FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
ENV REDIS_URL=redis://cache:6379/0
CMD ["python", "-m", "worker"]

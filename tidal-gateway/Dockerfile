FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY gateway.py .
ENV PORT=8081 SESSION_FILE=/config/tidal-session.json
VOLUME ["/config"]
EXPOSE 8081
CMD ["python", "gateway.py"]

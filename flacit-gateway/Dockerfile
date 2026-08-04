FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY gateway.py matching.py ./
ENV PORT=8082 SESSION_FILE=/config/telegram-session.session
VOLUME ["/config"]
EXPOSE 8082
CMD ["python", "gateway.py"]

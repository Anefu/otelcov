# One image for the vLLM stand-in (app.py) and the Alertmanager webhook sink
# (sink.py).
FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY app.py sink.py ./
ENV PYTHONUNBUFFERED=1
CMD ["python", "app.py"]
